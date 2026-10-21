package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/codemowers/lolcatz/services/internal/auth"
	"github.com/codemowers/lolcatz/services/internal/platform"
	gooidc "github.com/coreos/go-oidc/v3/oidc"
	_ "github.com/lib/pq"
)

var (
	db       *sql.DB
	verifier *gooidc.IDTokenVerifier
	devToken = os.Getenv("DEV_AUTH_TOKEN")
)

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("required env var %s is not set", k)
	}
	return v
}

func getEnvOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(0)
	var err error

	if devToken == "" {
		provider, err := gooidc.NewProvider(context.Background(), mustEnv("OIDC_ISSUER"))
		if err != nil {
			log.Fatalf("oidc provider: %v", err)
		}
		verifier = provider.Verifier(&gooidc.Config{ClientID: mustEnv("OIDC_AUDIENCE")})
	} else {
		log.Printf("WARNING: development authentication is enabled")
	}

	db, err = sql.Open("postgres", mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	if err = platform.PingDatabase(db); err != nil {
		log.Fatalf("db ping: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/comments/images/{id}/comments", requireAuth("lolcatz:comments:write", handlePost))

	if err := platform.Serve(mux); err != nil {
		log.Fatal(err)
	}
}

func requireAuth(requiredScope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || raw == "" {
			auth.InvalidToken(w)
			return
		}
		subject := ""
		if devToken != "" {
			if raw != devToken {
				auth.InvalidToken(w)
				return
			}
		} else {
			token, valid := auth.Verify(w, r, verifier, raw, requiredScope)
			if !valid {
				return
			}
			subject = token.Subject
		}
		authenticated, valid := auth.AuthenticateUser(w, r, db, subject, devToken != "")
		if valid {
			next(w, authenticated)
		}
	}
}

func handlePost(w http.ResponseWriter, r *http.Request) {
	imageID := r.PathValue("id")
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Body == "" {
		http.Error(w, "body required", 400)
		return
	}
	author := auth.RequestUser(r).Name
	if author == "" {
		author = "Anonymous"
	}

	var id int64
	err := db.QueryRowContext(r.Context(),
		`INSERT INTO comments (image_id, body, author, user_id) VALUES ($1,$2,$3,$4) RETURNING id`,
		imageID, body.Body, author, auth.RequestUser(r).ID,
	).Scan(&id)
	if err != nil {
		log.Printf("db insert: %v", err)
		http.Error(w, "db error", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"id": id})
}
