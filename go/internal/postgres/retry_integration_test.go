//go:build integration

package postgres

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	pool, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRetryTransactionRecoversRealSerializationFailure(t *testing.T) {
	pool := integrationPool(t)
	if _, err := pool.Exec(t.Context(), `DROP TABLE IF EXISTS retry_serialization;
		CREATE TABLE retry_serialization (id integer PRIMARY KEY, value integer NOT NULL);
		INSERT INTO retry_serialization VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS retry_serialization`) })
	var first sync.WaitGroup
	first.Add(2)
	var attempts atomic.Int32
	worker := func() error {
		_, err := RetryTransaction(t.Context(), func() (struct{}, error) {
			attempt := attempts.Add(1)
			tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
			if err != nil {
				return struct{}{}, err
			}
			defer tx.Rollback(t.Context())
			var value int
			if err := tx.QueryRow(t.Context(), `SELECT value FROM retry_serialization WHERE id=1`).Scan(&value); err != nil {
				return struct{}{}, err
			}
			if attempt <= 2 {
				first.Done()
				first.Wait()
			}
			if _, err := tx.Exec(t.Context(), `UPDATE retry_serialization SET value=$1 WHERE id=1`, value+1); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, tx.Commit(t.Context())
		})
		return err
	}
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- worker() }()
	go func() { errorsCh <- worker() }()
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
	}
	var value int
	if err := pool.QueryRow(t.Context(), `SELECT value FROM retry_serialization WHERE id=1`).Scan(&value); err != nil || value != 2 || attempts.Load() < 3 {
		t.Fatalf("value=%d attempts=%d err=%v", value, attempts.Load(), err)
	}
}

func TestRetryTransactionRecoversRealDeadlock(t *testing.T) {
	pool := integrationPool(t)
	if _, err := pool.Exec(t.Context(), `DROP TABLE IF EXISTS retry_deadlock;
		CREATE TABLE retry_deadlock (id integer PRIMARY KEY, value integer NOT NULL);
		INSERT INTO retry_deadlock VALUES (1, 0), (2, 0)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS retry_deadlock`) })
	var first sync.WaitGroup
	first.Add(2)
	var deadlockSeen atomic.Bool
	worker := func(firstID, secondID int) error {
		attempt := 0
		_, err := RetryTransaction(t.Context(), func() (struct{}, error) {
			attempt++
			tx, err := pool.Begin(t.Context())
			if err != nil {
				return struct{}{}, err
			}
			defer tx.Rollback(t.Context())
			if _, err := tx.Exec(t.Context(), `UPDATE retry_deadlock SET value=value+1 WHERE id=$1`, firstID); err != nil {
				return struct{}{}, err
			}
			if attempt == 1 {
				first.Done()
				first.Wait()
			}
			if _, err := tx.Exec(t.Context(), `UPDATE retry_deadlock SET value=value+1 WHERE id=$1`, secondID); err != nil {
				var postgresError *pgconn.PgError
				if errors.As(err, &postgresError) && postgresError.Code == "40P01" {
					deadlockSeen.Store(true)
				}
				return struct{}{}, err
			}
			return struct{}{}, tx.Commit(t.Context())
		})
		return err
	}
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- worker(1, 2) }()
	go func() { errorsCh <- worker(2, 1) }()
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
	}
	if !deadlockSeen.Load() {
		t.Fatal("real SQLSTATE 40P01 was not observed")
	}
	rows, err := pool.Query(t.Context(), `SELECT value FROM retry_deadlock ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var value int
		if err := rows.Scan(&value); err != nil || value != 2 {
			t.Fatalf("value=%d err=%v", value, err)
		}
	}
}
