package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/codemowers/lolcatz/services/internal/platform"
	_ "github.com/lib/pq"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/redis/go-redis/v9"
)

var (
	db          *sql.DB
	minioClient *minio.Client
	bucket      string
	cache       *redis.Client
)

type Image struct {
	CommentCount int64        `json:"comment_count"`
	UserID       *int64       `json:"user_id"`
	Annotations  []Annotation `json:"annotations"`
	ID           string       `json:"id"`
	Board        string       `json:"board"`
	Title        string       `json:"title"`
	Filename     string       `json:"filename"`
	ContentType  string       `json:"content_type"`
	Size         int64        `json:"size"`
	Author       string       `json:"author"`
	Tags         []Tag        `json:"tags"`
	UploadedAt   time.Time    `json:"uploaded_at"`
	// When the photo was taken, per EXIF. Null when the file carried none.
	CapturedAt *time.Time `json:"captured_at"`
	Exif       *ExifInfo  `json:"exif"`
	OCR        *OCRInfo   `json:"ocr"`
	ImageURL   string     `json:"image_url"`
}

type Annotation struct {
	ID         int64      `json:"id"`
	Label      string     `json:"label"`
	Confidence float64    `json:"confidence"`
	BBox       [4]float64 `json:"bbox"`
}

type Tag struct {
	Name       string  `json:"name"`
	Confidence float64 `json:"confidence"`
}

type Board struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Icon  string `json:"icon"`
	Blurb string `json:"blurb"`
	Count int64  `json:"count"`
}

// ExifInfo is produced by the exif service. Every field is optional because
// cameras write wildly different subsets of the spec.
type ExifInfo struct {
	CameraMake      *string  `json:"camera_make"`
	CameraModel     *string  `json:"camera_model"`
	LensModel       *string  `json:"lens_model"`
	Software        *string  `json:"software"`
	Width           *int     `json:"width"`
	Height          *int     `json:"height"`
	ISO             *int     `json:"iso"`
	FNumber         *float64 `json:"f_number"`
	ExposureSeconds *float64 `json:"exposure_seconds"`
	FocalLengthMM   *float64 `json:"focal_length_mm"`
	GPSLatitude     *float64 `json:"gps_latitude"`
	GPSLongitude    *float64 `json:"gps_longitude"`
	GPSAltitudeM    *float64 `json:"gps_altitude_m"`
	ProducerVersion string   `json:"producer_version"`
}

// OCRInfo is produced by the ocr service.
type OCRInfo struct {
	Text            string  `json:"text"`
	Language        *string `json:"language"`
	ProducerVersion string  `json:"producer_version"`
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
	log.SetFlags(0)
	var err error
	db, err = sql.Open("postgres", mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	if err = platform.PingDatabase(db); err != nil {
		log.Fatalf("db ping: %v", err)
	}
	cache = redis.NewClient(&redis.Options{Addr: getEnvOr("REDIS_ADDR", "lolcatz-redis:6379"), Password: os.Getenv("REDIS_PASSWORD"), TLSConfig: platform.ClientTLS(os.Getenv("REDIS_TLS") == "true")})

	bucket = mustEnv("S3_BUCKET")
	minioClient, err = minio.New(mustEnv("S3_ENDPOINT"), &minio.Options{
		Creds:  credentials.NewStaticV4(mustEnv("S3_ACCESS_KEY"), mustEnv("S3_SECRET_KEY"), ""),
		Secure: mustEnv("S3_USE_SSL") == "true",
		Region: getEnvOr("S3_REGION", "us-east-1"),
	})
	if err != nil {
		log.Fatalf("minio: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/browse/boards/counts", handleBoardCounts)
	mux.HandleFunc("GET /api/browse/boards", handleBoards)
	mux.HandleFunc("GET /api/browse/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"admin": getEnvOr("ADMIN_ENABLED", "false") == "true"})
	})
	mux.HandleFunc("GET /api/browse/boards/{board}", handleBoard)
	mux.HandleFunc("GET /api/browse/nearby", handleNearby)
	mux.HandleFunc("GET /api/browse/users/{id}", handleUser)
	mux.HandleFunc("GET /api/browse/media/{id}", handleMedia)
	mux.HandleFunc("GET /api/browse/images/{id}", handleImage)

	if err := platform.Serve(mux); err != nil {
		log.Fatal(err)
	}
}

func handleNearby(w http.ResponseWriter, r *http.Request) {
	lat, errLat := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
	lon, errLon := strconv.ParseFloat(r.URL.Query().Get("lon"), 64)
	radius, errRadius := strconv.ParseFloat(r.URL.Query().Get("radius"), 64)
	if errLat != nil || errLon != nil || errRadius != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 || radius <= 0 || radius > 100000 {
		http.Error(w, "lat, lon and radius (metres) required", http.StatusBadRequest)
		return
	}
	rows, err := db.QueryContext(r.Context(), `
		SELECT e.image_id,
		       ST_Distance(e.location::geography,
		         ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography) AS distance_m
		FROM image_exif e
		WHERE e.location IS NOT NULL
		  AND ST_DWithin(e.location::geography,
		    ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography, $3)
		ORDER BY distance_m LIMIT 100`, lon, lat, radius)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type nearbyImage struct {
		ImageID   string  `json:"image_id"`
		DistanceM float64 `json:"distance_m"`
	}
	results := []nearbyImage{}
	for rows.Next() {
		var item nearbyImage
		if err := rows.Scan(&item.ImageID, &item.DistanceM); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func handleBoards(w http.ResponseWriter, r *http.Request) {
	generation, _ := cache.Get(r.Context(), "lolcatz:boards:generation").Result()
	cacheKey := "lolcatz:boards:v2:" + generation
	if value, err := cache.Get(r.Context(), cacheKey).Bytes(); err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write(value)
		return
	}
	rows, err := db.QueryContext(r.Context(), `
		SELECT b.id, b.name, b.icon, b.blurb, COUNT(i.id)
		FROM boards b LEFT JOIN images i ON i.board = b.id
		GROUP BY b.id ORDER BY COUNT(i.id) DESC, b.id`)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	boards := []Board{}
	for rows.Next() {
		var board Board
		if err := rows.Scan(&board.ID, &board.Name, &board.Icon, &board.Blurb, &board.Count); err != nil {
			http.Error(w, "db error", 500)
			return
		}
		boards = append(boards, board)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "db error", 500)
		return
	}
	payload, err := json.Marshal(boards)
	if err != nil {
		http.Error(w, "encode error", 500)
		return
	}
	if err := cache.Set(r.Context(), cacheKey, payload, time.Minute).Err(); err != nil {
		log.Printf("cache boards: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload)
}

func handleBoardCounts(w http.ResponseWriter, r *http.Request) {
	rows, err := db.QueryContext(r.Context(), `
		SELECT board, COUNT(*) FROM images GROUP BY board
	`)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	counts := map[string]int64{}
	for rows.Next() {
		var board string
		var count int64
		if err := rows.Scan(&board, &count); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		counts[board] = count
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(counts)
}

func presign(ctx context.Context, board, id, downloadName string) string {
	objectKey := board + "/" + id
	responseHeaders := url.Values{
		"response-cache-control": {"public, max-age=31536000, immutable"},
	}
	if downloadName != "" {
		responseHeaders.Set("response-content-disposition", mime.FormatMediaType("attachment", map[string]string{"filename": downloadName}))
	}
	u, err := minioClient.PresignedGetObject(ctx, bucket, objectKey, 7*24*time.Hour, responseHeaders)
	if err != nil {
		return ""
	}
	return u.String()
}

func mediaURL(id string) string {
	return "/api/browse/media/" + url.PathEscape(id)
}

func handleMedia(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var board, filename string
	if err := db.QueryRowContext(r.Context(), `SELECT board, filename FROM images WHERE id = $1`, id).Scan(&board, &filename); err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	downloadName := ""
	if r.URL.Query().Get("download") == "1" {
		downloadName = filename
	}
	location := presign(r.Context(), board, id, downloadName)
	if location == "" {
		http.Error(w, "could not sign image URL", http.StatusBadGateway)
		return
	}

	// Reuse the signed URL for a day instead of issuing a new browser cache key
	// every few minutes. Seven-day signatures outlive cached redirects even if
	// the browser evicts the image body. Uploaded IDs never change.
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	http.Redirect(w, r, location, http.StatusFound)
}

// scanImage reads one row of the shared projection. EXIF and OCR come from
// LEFT JOINs, so their sub-objects stay nil until the matching consumer has
// actually produced something for this image.
func scanImage(scan func(...any) error, img *Image) error {
	var exifInfo ExifInfo
	var ocrInfo OCRInfo
	var exifVersion, ocrVersion, ocrText *string
	var tagsJSON, annotationsJSON []byte

	if err := scan(&img.ID, &img.Board, &img.Title, &img.Filename,
		&img.ContentType, &img.Size, &img.Author, &img.UserID, &tagsJSON, &annotationsJSON, &img.UploadedAt, &img.CommentCount,
		&img.CapturedAt, &exifInfo.CameraMake, &exifInfo.CameraModel, &exifInfo.LensModel,
		&exifInfo.Software, &exifInfo.Width, &exifInfo.Height, &exifInfo.ISO,
		&exifInfo.FNumber, &exifInfo.ExposureSeconds, &exifInfo.FocalLengthMM,
		&exifInfo.GPSLatitude, &exifInfo.GPSLongitude, &exifInfo.GPSAltitudeM, &exifVersion,
		&ocrText, &ocrInfo.Language, &ocrVersion); err != nil {
		return err
	}
	if err := json.Unmarshal(tagsJSON, &img.Tags); err != nil {
		return err
	}

	if err := json.Unmarshal(annotationsJSON, &img.Annotations); err != nil {
		return err
	}

	if exifVersion != nil {
		exifInfo.ProducerVersion = *exifVersion
		img.Exif = &exifInfo
	}
	if ocrVersion != nil {
		ocrInfo.ProducerVersion = *ocrVersion
		if ocrText != nil {
			ocrInfo.Text = *ocrText
		}
		img.OCR = &ocrInfo
	}
	img.ImageURL = mediaURL(img.ID)
	return nil
}

func scanImages(rows *sql.Rows) ([]Image, error) {
	images := []Image{}
	for rows.Next() {
		var img Image
		if err := scanImage(rows.Scan, &img); err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return images, nil
}

func handleBoard(w http.ResponseWriter, r *http.Request) {
	board := r.PathValue("board")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	where := ` WHERE i.board = $1`
	args := []any{board, limit}
	before, beforeID := r.URL.Query().Get("before"), r.URL.Query().Get("before_id")
	if before != "" || beforeID != "" {
		timestamp, err := time.Parse(time.RFC3339Nano, before)
		if err != nil || beforeID == "" {
			http.Error(w, "before and before_id must identify an upload timestamp and image", http.StatusBadRequest)
			return
		}
		where += ` AND (i.uploaded_at, i.id) < ($3, $4)`
		args = append(args, timestamp, beforeID)
	}

	rows, err := db.QueryContext(r.Context(), imageProjection+where+` GROUP BY i.id, u.name, e.image_id, o.image_id ORDER BY i.uploaded_at DESC, i.id DESC LIMIT $2`, args...)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	defer rows.Close()

	images, err := scanImages(rows)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(images)
}

func handleImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var img Image
	row := db.QueryRowContext(r.Context(), imageProjection+` WHERE i.id = $1 GROUP BY i.id, u.name, e.image_id, o.image_id`, id)
	err := scanImage(row.Scan, &img)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	rows, err := db.QueryContext(r.Context(), `
		SELECT c.id, c.body, COALESCE(u.name, c.author), c.user_id, c.created_at
		FROM comments c LEFT JOIN users u ON u.id = c.user_id
		WHERE c.image_id=$1 ORDER BY c.created_at ASC
	`, id)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	defer rows.Close()

	type Comment struct {
		UserID    *int64    `json:"user_id"`
		ID        int64     `json:"id"`
		Body      string    `json:"body"`
		Author    string    `json:"author"`
		CreatedAt time.Time `json:"created_at"`
	}
	comments := []Comment{}
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.Body, &c.Author, &c.UserID, &c.CreatedAt); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		comments = append(comments, c)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"image": img, "comments": comments})
}

const imageProjection = `
		SELECT i.id, i.board,
		       COALESCE(NULLIF(BTRIM(i.title), ''), o.derived_title, '') AS title,
		       i.filename, i.content_type, i.size,
		       COALESCE(u.name, i.author) AS author, i.user_id,
		       COALESCE(json_agg(json_build_object('name', t.tag, 'confidence', t.confidence) ORDER BY t.confidence DESC, t.tag) FILTER (WHERE t.tag IS NOT NULL), '[]') AS tags,
               COALESCE((SELECT json_agg(json_build_object(
                   'id', a.id, 'label', a.label, 'confidence', a.confidence,
                   'bbox', json_build_array(a.x1, a.y1, a.x2, a.y2)) ORDER BY a.id)
                   FROM image_annotations a WHERE a.image_id = i.id), '[]') AS annotations,
		       i.uploaded_at,
               (SELECT COUNT(*) FROM comments c WHERE c.image_id = i.id) AS comment_count,
		       e.captured_at, e.camera_make, e.camera_model, e.lens_model, e.software,
		       e.width, e.height, e.iso, e.f_number, e.exposure_seconds, e.focal_length_mm,
		       ST_Y(e.location), ST_X(e.location), ST_Z(e.location), e.producer_version,
		       o.text, o.language, o.producer_version
		FROM images i
		LEFT JOIN users      u ON u.id = i.user_id
		LEFT JOIN (SELECT image_id, label AS tag, MAX(confidence) AS confidence FROM image_annotations GROUP BY image_id, label) t ON t.image_id = i.id
		LEFT JOIN image_exif e ON e.image_id = i.id
		LEFT JOIN image_ocr  o ON o.image_id = i.id`

func handleUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "not found", 404)
		return
	}
	var user struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	err = db.QueryRowContext(r.Context(), "SELECT id, name FROM users WHERE id = $1", id).Scan(&user.ID, &user.Name)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	rows, err := db.QueryContext(r.Context(), imageProjection+` WHERE i.user_id = $1 GROUP BY i.id, u.name, e.image_id, o.image_id ORDER BY i.uploaded_at DESC, i.id LIMIT 25 OFFSET $2`, id, offset)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	defer rows.Close()
	images, err := scanImages(rows)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"user": user, "images": images})
}
