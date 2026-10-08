package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestBrowseWithWorkersDisabled(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	uri, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	schemaName := fmt.Sprintf("browse_core_%d", time.Now().UnixNano())
	query := uri.Query()
	query.Set("search_path", schemaName+",public")
	uri.RawQuery = query.Encode()
	connection, err := sql.Open("postgres", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec("CREATE SCHEMA " + schemaName); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	previous := db
	db = connection
	t.Cleanup(func() {
		db = previous
		if _, err := connection.Exec("DROP SCHEMA " + schemaName + " CASCADE"); err != nil {
			t.Error(err)
		}
		connection.Close()
	})
	schemaSQL, err := os.ReadFile("../uploader/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(string(schemaSQL)); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(`INSERT INTO images (id, filename, content_type) VALUES ('core-only', 'cat.jpg', 'image/jpeg')`); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/api/browse/boards/b", nil)
	request.SetPathValue("board", "b")
	response := httptest.NewRecorder()
	handleBoard(response, request)
	if response.Code != 200 {
		t.Fatalf("board without workers: %d %s", response.Code, response.Body)
	}
	var images []Image
	if err := json.Unmarshal(response.Body.Bytes(), &images); err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].ID != "core-only" || images[0].OCR != nil || images[0].Exif != nil || len(images[0].Tags) != 0 || len(images[0].Annotations) != 0 {
		t.Fatalf("unexpected unenriched image: %+v", images)
	}
	request = httptest.NewRequest("GET", "/api/browse/images/core-only", nil)
	request.SetPathValue("id", "core-only")
	response = httptest.NewRecorder()
	handleImage(response, request)
	if response.Code != 200 {
		t.Fatalf("image without workers: %d %s", response.Code, response.Body)
	}
}
