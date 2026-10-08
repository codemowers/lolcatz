package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codemowers/lolcatz/services/internal/auth"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
)

func uploadDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("upload_test_%d", time.Now().UnixNano())
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	connection, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec("CREATE SCHEMA " + schema); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := connection.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Error(err)
		}
		connection.Close()
	})
	if err := initSchema(connection); err != nil {
		t.Fatal(err)
	}
	return connection
}

type uploadTransport func(*http.Request) (*http.Response, error)

func (f uploadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type publishFunc func(context.Context, ...kafka.Message) error

func (f publishFunc) WriteMessages(ctx context.Context, messages ...kafka.Message) error {
	return f(ctx, messages...)
}

func TestConfirmOwnershipIdempotencyAndDurableDelivery(t *testing.T) {
	connection := uploadDatabase(t)
	oldDB, oldMinio, oldCache := db, minioClient, cache
	db = connection
	t.Cleanup(func() { db, minioClient, cache = oldDB, oldMinio, oldCache })
	cache = redis.NewClient(&redis.Options{MaxRetries: -1, Dialer: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("cache unavailable in test")
	}})
	t.Cleanup(func() { cache.Close() })
	for _, subject := range []string{"alice", "bob"} {
		if _, err := db.Exec(`INSERT INTO users (subject, name) VALUES ($1, $1)`, subject); err != nil {
			t.Fatal(err)
		}
	}
	// Real MinIO signing and response parsing with an in-memory storage transport.
	objects := map[string]http.Header{}
	var err error
	minioClient, err = minio.New("storage.example.test", &minio.Options{
		Creds: credentials.NewStaticV4("test", "test-secret", ""), Region: "us-east-1",
		Transport: uploadTransport(func(r *http.Request) (*http.Response, error) {
			headers := objects[r.URL.Path]
			status := http.StatusOK
			switch r.Method {
			case http.MethodHead:
				if headers == nil {
					status = http.StatusNotFound
				}
			case http.MethodDelete:
				delete(objects, r.URL.Path)
				status = http.StatusNoContent
			default:
				t.Fatalf("unexpected storage request: %s %s", r.Method, r.URL)
			}
			return &http.Response{StatusCode: status, Header: headers.Clone(), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(subject string, handler http.HandlerFunc, method, body, id string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/", strings.NewReader(body))
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		r, ok := auth.AuthenticateUser(w, r, db, subject, false)
		if !ok {
			t.Fatalf("authenticate: %s", w.Body.String())
		}
		handler(w, r)
		return w
	}
	presigned := call("alice", handlePresign, "POST", `{"board":"b","filename":"cat.png","content_type":"image/png"}`, "")
	if presigned.Code != 200 {
		t.Fatalf("presign: %d %s", presigned.Code, presigned.Body.String())
	}
	var upload struct {
		ID      string            `json:"id"`
		URL     string            `json:"put_url"`
		Headers map[string]string `json:"put_headers"`
	}
	if err := json.Unmarshal(presigned.Body.Bytes(), &upload); err != nil {
		t.Fatal(err)
	}
	putURL, err := url.Parse(upload.URL)
	if err != nil {
		t.Fatal(err)
	}
	signedHeaders := putURL.Query().Get("X-Amz-SignedHeaders")
	for _, header := range []string{"content-type", "x-amz-meta-lolcatz-owner"} {
		if !strings.Contains(signedHeaders, header) {
			t.Fatalf("%s is not signed: %s", header, signedHeaders)
		}
	}
	if upload.Headers["X-Amz-Meta-Lolcatz-Owner"] == "" {
		t.Fatal("missing owner header")
	}
	stored := http.Header{}
	for key, value := range upload.Headers {
		stored.Set(key, value)
	}
	stored.Set("Content-Length", "7")
	stored.Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	objects[putURL.Path] = stored
	body := fmt.Sprintf(`{"id":%q,"board":"b","title":"Original","filename":"cat.png","content_type":"text/plain"}`, upload.ID)
	assertStatus := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("got %d, want %d: %s", w.Code, want, w.Body.String())
		}
	}
	// Another account cannot claim an uploaded object before confirmation.
	assertStatus(call("bob", handleConfirm, "POST", body, ""), 404)
	assertStatus(call("alice", handleConfirm, "POST", body, ""), 201)
	// Neither another account nor an owner retry can replace the retained event.
	assertStatus(call("bob", handleConfirm, "POST", body, ""), 404)
	assertStatus(call("alice", handleConfirm, "POST", strings.Replace(body, "Original", "Changed", 1), ""), 201)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM image_outbox`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("outbox count=%d error=%v", count, err)
	}
	var payload []byte
	if err := db.QueryRow(`SELECT payload FROM image_outbox`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event ImageEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event.Title != "Original" || event.ContentType != "image/png" || event.UploadedAt.IsZero() {
		t.Fatalf("noncanonical event: %+v", event)
	}

	// A broker outage leaves the event durable; deleting the post queues a
	// tombstone behind it instead of losing either message.
	failed := publishFunc(func(context.Context, ...kafka.Message) error { return errors.New("broker offline") })
	if sent, err := dispatchEvent(context.Background(), db, failed); sent || err == nil {
		t.Fatalf("sent=%v error=%v", sent, err)
	}
	assertStatus(call("alice", handleDeleteImage, "DELETE", "", upload.ID), 204)
	var delivered []kafka.Message
	writer := publishFunc(func(_ context.Context, messages ...kafka.Message) error {
		delivered = append(delivered, messages...)
		return nil
	})
	for range 2 {
		if sent, err := dispatchEvent(context.Background(), db, writer); !sent || err != nil {
			t.Fatalf("sent=%v error=%v", sent, err)
		}
	}
	if len(delivered) != 2 || string(delivered[0].Value) != string(payload) || delivered[1].Value != nil || string(delivered[1].Key) != upload.ID {
		t.Fatalf("incorrect delivery order: %+v", delivered)
	}
	if sent, err := dispatchEvent(context.Background(), db, writer); sent || err != nil {
		t.Fatalf("outbox not empty: sent=%v error=%v", sent, err)
	}
	// A stale confirmation cannot recreate a deleted post from a missing object.
	assertStatus(call("alice", handleConfirm, "POST", body, ""), 404)
}

func TestOutboxFailureRollsBackImage(t *testing.T) {
	database := uploadDatabase(t)
	var user auth.User
	if err := database.QueryRow(`INSERT INTO users (name) VALUES ('Alice') RETURNING id, name`).Scan(&user.ID, &user.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`ALTER TABLE image_outbox ADD CONSTRAINT reject_events CHECK (false)`); err != nil {
		t.Fatal(err)
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	event := ImageEvent{ID: uuid.NewString(), Board: "b", Filename: "cat.png", ContentType: "image/png"}
	if err := insertUpload(context.Background(), tx, event, 7, user, "test"); err == nil {
		t.Fatal("outbox failure was ignored")
	}
	tx.Rollback()
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial write: count=%d error=%v", count, err)
	}
}

func TestOutboxDispatchIsSerializedAcrossReplicas(t *testing.T) {
	database := uploadDatabase(t)
	if _, err := database.Exec(`INSERT INTO image_outbox (image_id, payload) VALUES ('first', NULL), ('second', NULL)`); err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		_, err := dispatchEvent(ctx, database, publishFunc(func(ctx context.Context, messages ...kafka.Message) error {
			close(entered)
			select {
			case <-release:
				return ctx.Err()
			case <-ctx.Done():
				return ctx.Err()
			}
		}))
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("dispatcher did not enter publisher")
	}
	// Cancel the first replica while its Kafka write is still in flight.
	// It must retain the lock until the publisher has actually returned.
	cancel()
	sent, err := dispatchEvent(context.Background(), database, publishFunc(func(context.Context, ...kafka.Message) error {
		t.Error("second replica published past an unacknowledged event")
		return nil
	}))
	close(release)
	if sent || err != nil {
		t.Fatalf("second replica sent=%v error=%v", sent, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
