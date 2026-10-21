package platform

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/lib/pq"
)

// PingDatabase allows a newly provisioned database two minutes to become ready.
// Each connection attempt is also bounded, so an unreachable host cannot hang startup.
func PingDatabase(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return pingDatabase(ctx, db, 2*time.Second)
}

func pingDatabase(ctx context.Context, db *sql.DB, delay time.Duration) error {
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := db.PingContext(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		var networkError net.Error
		var postgresError *pq.Error
		retryable := errors.As(err, &networkError) || errors.Is(err, context.DeadlineExceeded)
		if errors.As(err, &postgresError) {
			retryable = postgresError.Code.Class() == "08" || postgresError.Code == "57P03" || postgresError.Code == "53300"
		}
		if !retryable {
			return err
		}
		if ctx.Err() != nil {
			return fmt.Errorf("database startup retry budget exhausted: %w (last error: %v)", ctx.Err(), err)
		}
		log.Printf("database not ready (attempt %d); retrying in %s: %v", attempt, delay, err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("database startup retry budget exhausted: %w (last error: %v)", ctx.Err(), err)
		case <-timer.C:
		}
	}
}
