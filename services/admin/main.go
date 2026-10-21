package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/codemowers/lolcatz/services/internal/auth"
	"github.com/codemowers/lolcatz/services/internal/platform"
	gooidc "github.com/coreos/go-oidc/v3/oidc"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

var db *sql.DB
var verifier *gooidc.IDTokenVerifier
var cache *redis.Client
var devToken = os.Getenv("DEV_AUTH_TOKEN")
var boardID = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

func mustEnv(key string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	log.Fatalf("required env var %s is not set", key)
	return ""
}
func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func main() {
	var err error
	if devToken == "" {
		provider, err := gooidc.NewProvider(context.Background(), mustEnv("OIDC_ISSUER"))
		if err != nil {
			log.Fatal(err)
		}
		verifier = provider.Verifier(&gooidc.Config{ClientID: mustEnv("OIDC_AUDIENCE")})
	}
	db, err = sql.Open("postgres", mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	if err = platform.PingDatabase(db); err != nil {
		log.Fatal(err)
	}
	cache = redis.NewClient(&redis.Options{Addr: getEnv("REDIS_ADDR", "lolcatz-redis:6379"), Password: os.Getenv("REDIS_PASSWORD"), TLSConfig: platform.ClientTLS(os.Getenv("REDIS_TLS") == "true")})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/admin/me", requireAdmin(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"role":"admin"}`))
	}))
	mux.HandleFunc("POST /api/admin/boards", requireAdmin(createBoard))
	mux.HandleFunc("DELETE /api/admin/boards/{id}", requireAdmin(deleteBoard))

	if err := platform.Serve(mux); err != nil {
		log.Fatal(err)
	}
}

func requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || raw == "" {
			http.Error(w, "unauthorized", 401)
			return
		}
		if devToken != "" {
			if raw != devToken {
				http.Error(w, "unauthorized", 401)
				return
			}
			next(w, r)
			return
		}
		token, valid := auth.Verify(w, r, verifier, raw, "lolcatz:boards:write")
		if !valid {
			return
		}
		var claims struct {
			Groups []string `json:"groups"`
		}
		if token.Claims(&claims) != nil {
			http.Error(w, "invalid token claims", 401)
			return
		}
		for _, group := range claims.Groups {
			if group == "github.com:codemowers:admins" {
				next(w, r)
				return
			}
		}
		http.Error(w, "admin role required", http.StatusForbidden)
	}
}

func invalidate(ctx context.Context) {
	if err := cache.Incr(ctx, "lolcatz:boards:generation").Err(); err != nil {
		log.Printf("invalidate boards: %v", err)
	}
}

func createBoard(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var board struct{ ID, Name, Icon, Blurb string }
	if json.NewDecoder(r.Body).Decode(&board) != nil || !boardID.MatchString(board.ID) || strings.TrimSpace(board.Name) == "" || len(board.Name) > 100 || len(board.Icon) > 128 || len(board.Blurb) > 2000 || reservedBoard(board.ID) {
		http.Error(w, "id (lowercase alphanumeric) and name required", 400)
		return
	}
	if board.Icon == "" {
		board.Icon = "📌"
	}
	_, err := db.ExecContext(r.Context(), `INSERT INTO boards (id, name, icon, blurb) VALUES ($1,$2,$3,$4)`, board.ID, board.Name, board.Icon, board.Blurb)
	if err != nil {
		http.Error(w, "board already exists or could not be created", 409)
		return
	}
	invalidate(r.Context())
	w.WriteHeader(http.StatusCreated)
}

func reservedBoard(id string) bool {
	switch id {
	case "api", "search", "profile", "thread":
		return true
	}
	return false
}

func deleteBoard(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "b" {
		http.Error(w, "cannot delete the default board", http.StatusConflict)
		return
	}
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	defer tx.Rollback()
	// Serialize with uploads so the emptiness check and delete are one operation.
	if _, err := tx.ExecContext(r.Context(), `LOCK TABLE images IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		http.Error(w, "db error", 500)
		return
	}

	var count int
	if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM images WHERE board = $1`, id).Scan(&count); err != nil {
		http.Error(w, "db error", 500)
		return
	}
	if count != 0 {
		http.Error(w, "cannot delete a board with posts", http.StatusConflict)
		return
	}
	result, err := tx.ExecContext(r.Context(), `DELETE FROM boards WHERE id = $1`, id)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		http.Error(w, "not found", 404)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "db error", 500)
		return
	}
	invalidate(r.Context())
	w.WriteHeader(http.StatusNoContent)
}
