//go:build integration

package execution

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func executionIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	connectionString := os.Getenv("TEST_DATABASE_URL")
	if connectionString == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if err := Migrate(t.Context(), connectionString); err != nil {
		t.Fatalf("migrate execution ledger: %v", err)
	}
	pool, err := pgxpool.New(t.Context(), connectionString)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPostgresExecutionLedgerReplayConflictAndImmutability(t *testing.T) {
	pool := executionIntegrationPool(t)
	ledger := NewPostgresExecutionLedger(pool)
	start := ExecutionStart{
		ExecutionID:   uuid.NewString(),
		RequestDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Timestamp:     time.Now().UTC(), ReservationTTL: time.Minute,
	}
	handle, err := ledger.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	if _, replay := handle.Replay(); replay {
		t.Fatal("new reservation replayed")
	}
	result := Result{ExecutionID: start.ExecutionID, CandidateCommit: "0123456789012345678901234567890123456789"}
	if err := handle.Complete(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	replayed, err := ledger.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	value, replay := replayed.Replay()
	if !replay || value.ExecutionID != start.ExecutionID {
		t.Fatalf("unexpected replay: %#v %t", value, replay)
	}
	conflict := start
	conflict.RequestDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := ledger.Begin(t.Context(), conflict); !errors.Is(err, ErrExecutionConflict) {
		t.Fatalf("conflicting execution error = %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE execution_ledger
		SET completed_at=clock_timestamp() WHERE execution_id=$1`, start.ExecutionID); err == nil {
		t.Fatal("completed execution row was mutable")
	}
	if _, err := pool.Exec(
		t.Context(), `DELETE FROM execution_ledger WHERE execution_id=$1`, start.ExecutionID,
	); err == nil {
		t.Fatal("execution row was deletable")
	}
}

func TestPostgresExecutionReservationRecoveryAndConcurrentMigration(t *testing.T) {
	pool := executionIntegrationPool(t)
	connectionString := os.Getenv("TEST_DATABASE_URL")
	var wait sync.WaitGroup
	failures := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			failures <- Migrate(context.Background(), connectionString)
		}()
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var digest string
	if err := pool.QueryRow(
		t.Context(), `SELECT digest FROM schema_migrations WHERE version=9`,
	).Scan(&digest); err != nil || len(digest) != 64 {
		t.Fatalf("migration digest = %q error=%v", digest, err)
	}

	ledger := NewPostgresExecutionLedger(pool)
	start := ExecutionStart{
		ExecutionID:   uuid.NewString(),
		RequestDigest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Timestamp:     time.Now().UTC(), ReservationTTL: time.Minute,
	}
	if _, err := ledger.Begin(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE execution_ledger
		SET reserved_until=clock_timestamp()-interval '1 second'
		WHERE execution_id=$1`, start.ExecutionID); err != nil {
		t.Fatal(err)
	}
	recovered, err := ledger.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	if _, replay := recovered.Replay(); replay {
		t.Fatal("expired reservation replayed instead of recovering")
	}
	if err := recovered.Complete(t.Context(), Result{ExecutionID: start.ExecutionID}); err != nil {
		t.Fatal(err)
	}
}
