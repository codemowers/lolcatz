package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codemowers/lolcatz/services/internal/auth"
	gooidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

func loginTokens(t *testing.T) func(typ, audience, subject, email string, verified bool) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	oldVerifier, oldLoginVerifier, oldDevToken := verifier, loginVerifier, devToken
	t.Cleanup(func() { verifier, loginVerifier, devToken = oldVerifier, oldLoginVerifier, oldDevToken })
	keys := &gooidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}
	verifier = gooidc.NewVerifier("https://issuer.example/", keys, &gooidc.Config{ClientID: "https://app.example/api"})
	loginVerifier = gooidc.NewVerifier("https://issuer.example/", keys, &gooidc.Config{ClientID: "frontend-client"})
	devToken = ""
	return func(typ, audience, subject, email string, verified bool) string {
		claims := map[string]any{"iss": "https://issuer.example/", "sub": subject, "aud": audience,
			"exp": time.Now().Add(time.Hour).Unix(), "scope": "lolcatz:images:read lolcatz:images:write"}
		if email != "" {
			claims["email"], claims["email_verified"] = email, verified
		}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType(jose.ContentType(typ)))
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(claims)
		signed, err := signer.Sign(payload)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := signed.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
}

func loginRequest(access, identity string) *http.Request {
	body, _ := json.Marshal(map[string]string{"id_token": identity})
	req := httptest.NewRequest("POST", "/api/upload/session", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+access)
	return req
}

func TestLoginRejectsInvalidIdentity(t *testing.T) {
	sign := loginTokens(t)
	access := sign("at+jwt", "https://app.example/api", "alice", "", false)
	for _, tc := range []struct {
		name, audience, subject, email string
		verified                       bool
		want                           int
	}{
		{"wrong subject", "frontend-client", "mallory", "alice@example.test", true, 401},
		{"wrong audience", "other-client", "alice", "alice@example.test", true, 401},
		{"missing email", "frontend-client", "alice", "", true, 403},
		{"unverified email", "frontend-client", "alice", "alice@example.test", false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handleLogin(response, loginRequest(access, sign("JWT", tc.audience, tc.subject, tc.email, tc.verified)))
			if response.Code != tc.want {
				t.Fatalf("got %d: %s", response.Code, response.Body.String())
			}
		})
	}
}

// Run against local PostgreSQL with TEST_DATABASE_URL. The test creates and
// removes only its own schema; it never touches application tables.
func TestLoginThenUploadWithoutEmail(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	sign := loginTokens(t)
	schema := fmt.Sprintf("login_test_%d", time.Now().UnixNano())
	databaseURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := databaseURL.Query()
	query.Set("search_path", schema+",public")
	databaseURL.RawQuery = query.Encode()
	testDB, err := sql.Open("postgres", databaseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = testDB.Exec("CREATE SCHEMA " + schema); err != nil {
		testDB.Close()
		t.Fatal(err)
	}
	oldDB := db
	db = testDB
	t.Cleanup(func() {
		db = oldDB
		if _, err := testDB.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Error(err)
		}
		testDB.Close()
	})
	// Initialize an empty schema, then verify startup can safely run again.
	for range 2 {
		if err = initSchema(db); err != nil {
			t.Fatal(err)
		}
	}
	var ownerID int64
	access := sign("at+jwt", "https://app.example/api", "alice", "", false)
	identity := sign("JWT", "frontend-client", "alice", " Alice@Example.Test ", true)
	handler := requireAuth("lolcatz:images:write", func(w http.ResponseWriter, r *http.Request) {
		if auth.RequestUser(r).ID != ownerID {
			t.Error("resolved the wrong account")
		}
		w.WriteHeader(204)
	})
	requestUpload := func(token string, want int) {
		req := httptest.NewRequest("POST", "/api/upload/presign", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-User-Email", "alice@example.test")
		response := httptest.NewRecorder()
		handler(response, req)
		if response.Code != want {
			t.Fatalf("upload got %d: %s", response.Code, response.Body.String())
		}
	}
	requestUpload(access, 401) // Email headers cannot establish the missing link.
	for range 2 {
		response := httptest.NewRecorder()
		handleLogin(response, loginRequest(access, identity))
		if response.Code != 204 {
			t.Fatalf("login got %d: %s", response.Code, response.Body.String())
		}
	}
	if err = db.QueryRow("SELECT id FROM users WHERE email = 'alice@example.test'").Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO images
  (id, board, title, filename, content_type, size, author, user_agent, user_id)
  VALUES ('confirmed-post', 'b', 'test', 'new.png', 'image/png', 123, 'Alice', 'test-agent', $1)`, ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO comments (image_id, body, author, user_id)
  VALUES ('confirmed-post', 'reply', 'Alice', $1)`, ownerID); err != nil {
		t.Fatal(err)
	}
	if err = initSchema(db); err != nil {
		t.Fatal(err)
	}
	var postOwner, commentOwner int64
	var userAgent string
	if err = db.QueryRow(`SELECT i.user_id, c.user_id, i.user_agent FROM images i
  JOIN comments c ON c.image_id = i.id WHERE i.id = 'confirmed-post'`).Scan(&postOwner, &commentOwner, &userAgent); err != nil {
		t.Fatal(err)
	}
	if postOwner != ownerID || commentOwner != ownerID || userAgent != "test-agent" {
		t.Fatal("incorrect post ownership or user agent")
	}
	// Fresh schemas must enforce both board and user references.
	for _, query := range []string{
		"UPDATE images SET board = 'missing' WHERE id = 'confirmed-post'",
		"UPDATE images SET user_id = -1 WHERE id = 'confirmed-post'",
		"UPDATE comments SET user_id = -1 WHERE image_id = 'confirmed-post'",
	} {
		if _, err = db.Exec(query); err == nil {
			t.Fatalf("missing foreign key: %s", query)
		}
	}
	requestUpload(access, 204) // No email claim in the access token.
	requestUpload(sign("at+jwt", "https://app.example/api", "alice", "different@example.test", true), 204)
	requestUpload(sign("at+jwt", "https://app.example/api", "unlinked", "alice@example.test", true), 401)
	var users, links int
	if err = db.QueryRow("SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM users WHERE subject IS NOT NULL)").Scan(&users, &links); err != nil {
		t.Fatal(err)
	}
	if users != 1 || links != 1 {
		t.Fatalf("duplicate identities: users=%d links=%d", users, links)
	}
	// Email lookup preserves the local user when the verified login subject changes.
	otherAccess := sign("at+jwt", "https://app.example/api", "another-subject", "", false)
	response := httptest.NewRecorder()
	handleLogin(response, loginRequest(otherAccess, sign("JWT", "frontend-client", "another-subject", "alice@example.test", true)))
	if response.Code != 204 {
		t.Fatalf("second identity login got %d: %s", response.Code, response.Body.String())
	}
	requestUpload(otherAccess, 204)
	requestUpload(access, 401) // The former subject no longer identifies this account.
}
