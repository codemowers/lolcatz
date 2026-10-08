// Command exif consumes upload events for JPEG images, reads only the header
// bytes from S3, and writes the camera metadata back to Postgres.
//
// It is deliberately a separate consumer rather than part of the uploader: the
// derived data it produces is reproducible, so reprocessing is just a matter of
// replaying the topic (see README, "Reprocessing after a model change").
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/codemowers/lolcatz/services/internal/platform"
	_ "github.com/lib/pq"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/rwcarlsen/goexif/exif"
	"github.com/segmentio/kafka-go"
)

// Dimensions and the EXIF APP1 segment both sit in the opening bytes of a JPEG,
// so we range-read a header window instead of pulling the whole object.
const headerWindowBytes = 256 << 10

// producerVersion is recorded on every row this service writes. Bump it (or
// override via PRODUCER_VERSION) whenever extraction changes, so stale rows can
// be found with: SELECT image_id FROM image_exif WHERE producer_version <> ...
const defaultProducerVersion = "exif-2"

type ImageEvent struct {
	ID          string `json:"id"`
	Board       string `json:"board"`
	ContentType string `json:"content_type"`
}

var (
	db              *sql.DB
	minioClient     *minio.Client
	bucket          string
	producerVersion string
)

// This worker writes only image_exif; the core initializer creates its schema.
func init() { log.SetFlags(0) }

func mustEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return value
}

func getEnvOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func main() {
	platform.RegisterWorkerMetrics()
	stopMetrics, metricsErr := platform.StartMetrics()
	if metricsErr != nil {
		log.Fatal(metricsErr)
	}
	defer stopMetrics()
	var err error
	db, err = sql.Open("postgres", mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("db open: %v", err)
	}
	if err = db.Ping(); err != nil {
		log.Fatalf("db ping: %v", err)
	}
	defer db.Close()

	producerVersion = getEnvOr("PRODUCER_VERSION", defaultProducerVersion)
	bucket = mustEnv("S3_BUCKET")
	minioClient, err = minio.New(mustEnv("S3_ENDPOINT"), &minio.Options{
		Creds:  credentials.NewStaticV4(mustEnv("S3_ACCESS_KEY"), mustEnv("S3_SECRET_KEY"), ""),
		Secure: mustEnv("S3_USE_SSL") == "true",
		Region: getEnvOr("S3_REGION", "us-east-1"),
	})
	if err != nil {
		log.Fatalf("minio: %v", err)
	}

	reader := kafka.NewReader(kafka.ReaderConfig{
		Dialer:  &kafka.Dialer{SASLMechanism: platform.KafkaSASL(), Timeout: 10 * time.Second, TLS: platform.ClientTLS(os.Getenv("KAFKA_TLS") == "true")},
		Brokers: strings.Split(mustEnv("KAFKA_BROKERS"), ","),
		GroupID: getEnvOr("KAFKA_GROUP", "lolcatz-exif"),
		Topic:   getEnvOr("KAFKA_TOPIC", "lolcatz-images"),
		// Start from the beginning so a freshly seeked or brand new group
		// reprocesses the whole compacted log.
		StartOffset: kafka.FirstOffset,
	})
	defer reader.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("exif consuming %s as %s", reader.Config().Topic, reader.Config().GroupID)
	if err := consume(ctx, reader, processExisting, time.Second); err != nil && ctx.Err() == nil {
		// Exit without fetching later offsets; Kubernetes restarts at the failed event.
		reader.Close()
		log.Fatal(err)
	}
}

type eventReader interface {
	FetchMessage(context.Context) (kafka.Message, error)
	CommitMessages(context.Context, ...kafka.Message) error
}

func consume(ctx context.Context, reader eventReader, handle func(context.Context, ImageEvent) error, retryDelay time.Duration) error {
	for {
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		message, err := reader.FetchMessage(pollCtx)
		cancel()
		platform.WorkerPoll.SetToCurrentTime()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				platform.WorkerFailures.WithLabelValues("poll").Inc()
			}
			return err
		}
		if err := consumeMessage(ctx, reader, message, handle, retryDelay); err != nil {
			return err
		}
	}
}

func consumeMessage(ctx context.Context, reader eventReader, message kafka.Message, handle func(context.Context, ImageEvent) error, retryDelay time.Duration) error {
	start := time.Now()
	platform.WorkerStarted.SetToCurrentTime()
	defer func() { platform.WorkerStarted.Set(0); platform.WorkerDuration.Observe(time.Since(start).Seconds()) }()
	if len(message.Value) != 0 {
		var event ImageEvent
		if err := json.Unmarshal(message.Value, &event); err != nil || event.ID == "" {
			platform.WorkerSkipped.WithLabelValues("malformed").Inc()
			log.Printf("skipping malformed message at offset %d", message.Offset)
		} else if !strings.EqualFold(event.ContentType, "image/jpeg") {
			platform.WorkerSkipped.WithLabelValues("content_type").Inc()
		} else {
			for attempt := 0; ; attempt++ {
				err := handle(ctx, event)
				if err == nil {
					break
				}
				platform.WorkerFailures.WithLabelValues("process").Inc()
				if attempt == 2 || ctx.Err() != nil {
					return fmt.Errorf("process %s: %w", event.ID, err)
				}
				log.Printf("process %s failed; retrying: %v", event.ID, err)
				timer := time.NewTimer(retryDelay * time.Duration(1<<attempt))
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		}
	}
	if len(message.Value) == 0 {
		platform.WorkerSkipped.WithLabelValues("tombstone").Inc()
	}
	// Tombstones, malformed records and unsupported formats are skipped and committed.
	if err := reader.CommitMessages(ctx, message); err != nil {
		platform.WorkerFailures.WithLabelValues("commit").Inc()
		return err
	}
	platform.WorkerCommitted.Inc()
	platform.WorkerCommitTime.SetToCurrentTime()
	return nil
}

// Load canonical image metadata; a deleted image has no work left to do.
func processExisting(ctx context.Context, event ImageEvent) error {
	var board string
	err := db.QueryRowContext(ctx, "SELECT board FROM images WHERE id = $1 LIMIT 1", event.ID).Scan(&board)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	event.Board = board
	return process(ctx, event)
}

func process(ctx context.Context, event ImageEvent) error {
	board := event.Board
	if board == "" {
		board = "b"
	}
	objectKey := fmt.Sprintf("%s/%s", board, event.ID)

	head, err := readHeader(ctx, objectKey)
	if err != nil {
		return err
	}

	var fields exifFields
	if config, _, err := image.DecodeConfig(bytes.NewReader(head)); err == nil {
		fields.Width = &config.Width
		fields.Height = &config.Height
	}
	if data, err := exif.Decode(bytes.NewReader(head)); err == nil {
		fields.read(data)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO image_exif (
			image_id, captured_at, camera_make, camera_model, lens_model, software,
			width, height, orientation, iso, f_number, exposure_seconds,
			focal_length_mm, location,
			producer_version, processed_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,
			CASE WHEN $14::double precision IS NOT NULL AND $15::double precision IS NOT NULL
			     THEN ST_SetSRID(ST_MakePoint($15::double precision, $14::double precision, COALESCE($16::double precision, 0)), 4326)
			END,
			$17,NOW())
		ON CONFLICT (image_id) DO UPDATE SET
			captured_at = EXCLUDED.captured_at,
			camera_make = EXCLUDED.camera_make,
			camera_model = EXCLUDED.camera_model,
			lens_model = EXCLUDED.lens_model,
			software = EXCLUDED.software,
			width = EXCLUDED.width,
			height = EXCLUDED.height,
			orientation = EXCLUDED.orientation,
			iso = EXCLUDED.iso,
			f_number = EXCLUDED.f_number,
			exposure_seconds = EXCLUDED.exposure_seconds,
			focal_length_mm = EXCLUDED.focal_length_mm,
			location = EXCLUDED.location,
			producer_version = EXCLUDED.producer_version,
			processed_at = NOW()
	`,
		event.ID, fields.CapturedAt, fields.CameraMake, fields.CameraModel,
		fields.LensModel, fields.Software, fields.Width, fields.Height,
		fields.Orientation, fields.ISO, fields.FNumber, fields.ExposureSeconds,
		fields.FocalLengthMM, fields.GPSLatitude, fields.GPSLongitude,
		fields.GPSAltitudeM, producerVersion,
	)
	if err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	log.Printf("enriched %s", event.ID)
	return nil
}

func readHeader(ctx context.Context, objectKey string) ([]byte, error) {
	options := minio.GetObjectOptions{}
	if err := options.SetRange(0, headerWindowBytes-1); err != nil {
		return nil, fmt.Errorf("set range: %w", err)
	}
	object, err := minioClient.GetObject(ctx, bucket, objectKey, options)
	if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}
	defer object.Close()

	// A ranged object is not reliably seekable, so read the window once and
	// decode from memory. Bounded at 256 KiB.
	head, err := io.ReadAll(object)
	if err != nil {
		return nil, fmt.Errorf("read object: %w", err)
	}
	if len(head) == 0 {
		return nil, fmt.Errorf("empty object")
	}
	return head, nil
}
