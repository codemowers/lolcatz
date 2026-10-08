package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/codemowers/lolcatz/services/internal/auth"
	"github.com/codemowers/lolcatz/services/internal/platform"
	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
)

var (
	db            *sql.DB
	minioClient   *minio.Client
	kafkaWriter   *kafka.Writer
	verifier      *gooidc.IDTokenVerifier
	loginVerifier *gooidc.IDTokenVerifier
	bucket        = mustEnv("S3_BUCKET")
	devToken      = os.Getenv("DEV_AUTH_TOKEN")
	cache         *redis.Client
)

func init() {
	log.SetFlags(0)
}

type ImageEvent struct {
	ID          string    `json:"id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	Board       string    `json:"board"`
	Title       string    `json:"title"`
	UploadedAt  time.Time `json:"uploaded_at"`
}

type UploadedImage struct {
	CommentCount int64           `json:"comment_count"`
	ID           string          `json:"id"`
	Board        string          `json:"board"`
	Title        string          `json:"title"`
	Filename     string          `json:"filename"`
	ContentType  string          `json:"content_type"`
	Tags         json.RawMessage `json:"tags"`
	Annotations  json.RawMessage `json:"annotations"`
	UploadedAt   time.Time       `json:"uploaded_at"`
	ImageURL     string          `json:"image_url"`
}

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
	var err error
	if devToken == "" {
		provider, err := gooidc.NewProvider(context.Background(), mustEnv("OIDC_ISSUER"))
		if err != nil {
			log.Fatalf("oidc provider: %v", err)
		}
		verifier = provider.Verifier(&gooidc.Config{ClientID: mustEnv("OIDC_AUDIENCE")})
		loginVerifier = provider.Verifier(&gooidc.Config{ClientID: mustEnv("OIDC_CLIENT_ID")})
	} else {
		log.Printf("WARNING: development authentication is enabled")
	}

	db, err = sql.Open("postgres", mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("db open: %v", err)
	}
	if err = db.Ping(); err != nil {
		log.Fatalf("db ping: %v", err)
	}
	// The database operator may still be installing PostGIS on a fresh install.
	for attempt := 1; ; attempt++ {
		if err = initSchema(db); err == nil {
			break
		}
		if attempt == 30 {
			log.Fatalf("initialize schema: %v", err)
		}
		log.Printf("initialize schema (retrying): %v", err)
		time.Sleep(2 * time.Second)
	}
	cache = redis.NewClient(&redis.Options{Addr: getEnvOr("REDIS_ADDR", "lolcatz-redis:6379"), Password: os.Getenv("REDIS_PASSWORD"), TLSConfig: platform.ClientTLS(os.Getenv("REDIS_TLS") == "true")})

	minioClient, err = minio.New(mustEnv("S3_ENDPOINT"), &minio.Options{
		Creds:  credentials.NewStaticV4(mustEnv("S3_ACCESS_KEY"), mustEnv("S3_SECRET_KEY"), ""),
		Secure: mustEnv("S3_USE_SSL") == "true",
		Region: getEnvOr("S3_REGION", "us-east-1"),
	})
	if err != nil {
		log.Fatalf("minio: %v", err)
	}

	kafkaWriter = newEventWriter("lolcatz-images")
	defer kafkaWriter.Close()
	stopOutbox := startOutbox(db, kafkaWriter)
	defer stopOutbox()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/upload/session", handleLogin)
	// Step 1: browser asks for a presigned PUT URL
	mux.HandleFunc("POST /api/upload/presign", requireAuth("lolcatz:images:write", handlePresign))
	mux.HandleFunc("PUT /api/upload/bulk/{filename}", requireAuth("lolcatz:images:write", handleBulk))
	mux.HandleFunc("PUT /api/upload/bulk", requireAuth("lolcatz:images:write", handleBulk))
	mux.HandleFunc("PUT /api/upload/bulk/", requireAuth("lolcatz:images:write", handleBulk))
	// Step 2: browser calls this after uploading to minio to register the image
	mux.HandleFunc("POST /api/upload/confirm", requireAuth("lolcatz:images:write", handleConfirm))
	mux.HandleFunc("GET /api/upload/me", requireAuth("lolcatz:images:read", handleCurrentUser))
	mux.HandleFunc("GET /api/upload/me/images", requireAuth("lolcatz:images:read", handleMyImages))
	mux.HandleFunc("DELETE /api/upload/images/{id}", requireAuth("lolcatz:images:write", handleDeleteImage))

	if err := platform.Serve(mux); err != nil {
		log.Fatal(err)
	}
}

// Compaction and tombstones require every event for a key in the same partition.
func newEventWriter(topic string) *kafka.Writer {
	return &kafka.Writer{
		Transport:    &kafka.Transport{SASL: platform.KafkaSASL(), TLS: platform.ClientTLS(os.Getenv("KAFKA_TLS") == "true")},
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		MaxAttempts:  1, // The durable outbox owns retries.
		BatchSize:    1,
		Addr:         kafka.TCP(strings.Split(mustEnv("KAFKA_BROKERS"), ",")...),
		Topic:        topic,
		Balancer:     &kafka.Hash{},
		RequiredAcks: kafka.RequireAll,
	}
}

func invalidateBoards(ctx context.Context) {
	if err := cache.Incr(ctx, "lolcatz:boards:generation").Err(); err != nil {
		log.Printf("invalidate boards cache: %v", err)
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

//go:embed schema.sql
var schema string

func initSchema(db *sql.DB) error {
	_, err := db.Exec(schema)
	return err
}

func handleCurrentUser(w http.ResponseWriter, r *http.Request) {
	user := auth.RequestUser(r)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"id": user.ID, "name": user.Name})
}

func handleMyImages(w http.ResponseWriter, r *http.Request) {
	rows, err := db.QueryContext(r.Context(), `
		SELECT i.id, i.board, i.title, i.filename, i.content_type,
		       COALESCE(json_agg(json_build_object('name', t.tag, 'confidence', t.confidence) ORDER BY t.confidence DESC, t.tag) FILTER (WHERE t.tag IS NOT NULL), '[]') AS tags,
               COALESCE((SELECT json_agg(json_build_object(
                   'id', a.id, 'label', a.label, 'confidence', a.confidence,
                   'bbox', json_build_array(a.x1, a.y1, a.x2, a.y2)) ORDER BY a.id)
                   FROM image_annotations a WHERE a.image_id = i.id), '[]') AS annotations,
		       i.uploaded_at,
               (SELECT COUNT(*) FROM comments c WHERE c.image_id = i.id) AS comment_count
		FROM images i LEFT JOIN (SELECT image_id, label AS tag, MAX(confidence) AS confidence FROM image_annotations GROUP BY image_id, label) t ON t.image_id = i.id
		WHERE i.user_id = $1
		GROUP BY i.id
		ORDER BY i.uploaded_at DESC
		LIMIT 100
	`, auth.RequestUser(r).ID)
	if err != nil {
		log.Printf("list own images: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	images := []UploadedImage{}
	for rows.Next() {
		var img UploadedImage
		if err := rows.Scan(&img.ID, &img.Board, &img.Title, &img.Filename,
			&img.ContentType, &img.Tags, &img.Annotations, &img.UploadedAt, &img.CommentCount); err != nil {
			log.Printf("scan own image: %v", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		img.ImageURL = "/api/browse/media/" + url.PathEscape(img.ID)
		images = append(images, img)
	}
	if err := rows.Err(); err != nil {
		log.Printf("iterate own images: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(images)
}

func handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	userID := auth.RequestUser(r).ID
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := lockImage(r.Context(), tx, id); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	var board string
	err = tx.QueryRowContext(r.Context(), `
		SELECT board FROM images WHERE id = $1 AND user_id = $2
	`, id, userID).Scan(&board)
	if err == sql.ErrNoRows {
		http.Error(w, "post not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("find post for deletion: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	objectKey := fmt.Sprintf("%s/%s", board, id)
	if err := minioClient.RemoveObject(r.Context(), bucket, objectKey, minio.RemoveObjectOptions{}); err != nil {
		log.Printf("delete object %s: %v", objectKey, err)
		http.Error(w, "storage error", http.StatusBadGateway)
		return
	}

	result, err := tx.ExecContext(r.Context(), `
		DELETE FROM images WHERE id = $1 AND user_id = $2
	`, id, userID)
	if err != nil {
		log.Printf("delete post %s: %v", id, err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	deleted, err := result.RowsAffected()
	if err != nil || deleted != 1 {
		http.Error(w, "post not found", http.StatusNotFound)
		return
	}
	if err := enqueueEvent(r.Context(), tx, id, nil); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	invalidateBoards(r.Context())

	// Thumbnail generation holds a shared lock on the image row. Once the
	// DELETE completes, no generator can recreate these cached derivatives.
	for _, variant := range []string{"256", "512", "preview"} {
		key := fmt.Sprintf("_thumbnails/v1/%s/%s.jpg", id, variant)
		if err := minioClient.RemoveObject(r.Context(), bucket, key, minio.RemoveObjectOptions{}); err != nil {
			log.Printf("delete thumbnail %s (non-fatal): %v", key, err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func handlePresign(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req struct {
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Board       string `json:"board"`
		Title       string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if !validUploadFields(w, req.Title, req.Filename) {
		return
	}

	allowed := map[string]bool{
		"image/jpeg": true, "image/png": true,
		"image/gif": true, "image/webp": true,
	}
	if !allowed[req.ContentType] {
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}
	if req.Board == "" {
		req.Board = "b"
	}

	if !requireBoard(w, r, req.Board) {
		return
	}
	id := uuid.New().String()
	objectKey := fmt.Sprintf("%s/%s", req.Board, id)

	// S3 verifies these headers against the signature. The client cannot change
	// the owner or media type, and confirmation reads them back from S3.
	headers := http.Header{}
	headers.Set("X-Amz-Meta-Lolcatz-Owner", strconv.FormatInt(auth.RequestUser(r).ID, 10))
	headers.Set("Content-Type", req.ContentType)
	putURL, err := minioClient.PresignHeader(
		r.Context(), http.MethodPut, bucket, objectKey, 15*time.Minute, nil, headers,
	)
	if err != nil {
		log.Printf("presign: %v", err)
		http.Error(w, "presign error", 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":      id,
		"put_url": putURL.String(),
		"put_headers": map[string]string{
			"X-Amz-Meta-Lolcatz-Owner": headers.Get("X-Amz-Meta-Lolcatz-Owner"),
			"Content-Type":             req.ContentType,
		},
		"board":    req.Board,
		"title":    req.Title,
		"filename": req.Filename,
	})
}

func handleBulk(w http.ResponseWriter, r *http.Request) {
	filename := r.PathValue("filename")
	contentType := r.Header.Get("Content-Type")
	size := r.ContentLength
	file := r.Body
	if filename == "" {
		filename = "upload-" + uuid.New().String() + ".jpg"
	}
	defer file.Close()
	filename = filepath.Base(filename)
	board := r.URL.Query().Get("board")
	if board == "" {
		board = "b"
	}
	if !requireBoard(w, r, board) {
		return
	}
	title := r.URL.Query().Get("title")
	if !validUploadFields(w, title, filename) {
		return
	}
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = mime.TypeByExtension(strings.ToLower(filepath.Ext(filename)))
	}
	allowed := map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}
	if !allowed[contentType] {
		http.Error(w, "unsupported content type", 415)
		return
	}
	id := uuid.New().String()
	key := fmt.Sprintf("%s/%s", board, id)
	info, err := minioClient.PutObject(r.Context(), bucket, key, file, size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		http.Error(w, "storage upload failed", 500)
		return
	}
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	defer tx.Rollback()
	event := ImageEvent{ID: id, Board: board, Title: title, Filename: filename, ContentType: contentType}
	if err := insertUpload(r.Context(), tx, event, info.Size, auth.RequestUser(r), r.UserAgent()); err != nil {
		http.Error(w, "db insert failed", 500)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "db error", 500)
		return
	}
	invalidateBoards(r.Context())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id": id, "filename": filename})
}

func handleConfirm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req struct {
		ID       string `json:"id"`
		Board    string `json:"board"`
		Title    string `json:"title"`
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if !validUploadFields(w, req.Title, req.Filename) {
		return
	}
	if id, err := uuid.Parse(req.ID); err != nil || id.String() != req.ID {
		http.Error(w, "invalid image id", http.StatusBadRequest)
		return
	}
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	defer tx.Rollback()
	if err := lockImage(r.Context(), tx, req.ID); err != nil {
		http.Error(w, "db error", 500)
		return
	}
	// A retry after a lost HTTP response is a no-op, including event publication.
	var owner int64
	err = tx.QueryRowContext(r.Context(), `SELECT user_id FROM images WHERE id = $1`, req.ID).Scan(&owner)
	if err == nil {
		if owner != auth.RequestUser(r).ID {
			http.Error(w, "upload not found", http.StatusNotFound)
			return
		}
		confirmed(w, req.ID)
		return
	}
	if err != sql.ErrNoRows {
		http.Error(w, "db error", 500)
		return
	}

	if !requireBoard(w, r, req.Board) {
		return
	}
	objectKey := fmt.Sprintf("%s/%s", req.Board, req.ID)

	// Verify object actually exists in minio
	info, err := minioClient.StatObject(r.Context(), bucket, objectKey, minio.StatObjectOptions{})
	if err != nil {
		log.Printf("stat object %s: %v", objectKey, err)
		http.Error(w, "object not found in storage", 404)
		return
	}
	if info.Metadata.Get("X-Amz-Meta-Lolcatz-Owner") != strconv.FormatInt(auth.RequestUser(r).ID, 10) {
		http.Error(w, "upload not found", http.StatusNotFound)
		return
	}
	event := ImageEvent{ID: req.ID, Board: req.Board, Title: req.Title, Filename: req.Filename, ContentType: info.ContentType}
	if err := insertUpload(r.Context(), tx, event, info.Size, auth.RequestUser(r), r.UserAgent()); err != nil {
		log.Printf("db insert: %v", err)
		http.Error(w, "db error", 500)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "db error", 500)
		return
	}
	invalidateBoards(r.Context())
	confirmed(w, req.ID)
}

func confirmed(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": id})
}

func requireBoard(w http.ResponseWriter, r *http.Request, board string) bool {
	var exists bool
	if err := db.QueryRowContext(r.Context(), `SELECT EXISTS (SELECT 1 FROM boards WHERE id=$1)`, board).Scan(&exists); err != nil {
		http.Error(w, "db error", 500)
		return false
	}
	if !exists {
		http.Error(w, "unknown board", 400)
		return false
	}
	return true
}
