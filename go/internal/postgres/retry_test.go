package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestRetryTransactionRetriesConflictsAndClassifiesExhaustion(t *testing.T) {
	attempts := 0
	value, err := RetryTransaction(t.Context(), func() (string, error) {
		attempts++
		if attempts < 3 {
			return "", &pgconn.PgError{Code: "40001"}
		}
		return "done", nil
	})
	if err != nil || value != "done" || attempts != 3 {
		t.Fatalf("value=%q attempts=%d err=%v", value, attempts, err)
	}
	attempts = 0
	_, err = RetryTransaction(t.Context(), func() (struct{}, error) {
		attempts++
		return struct{}{}, &pgconn.PgError{Code: "40P01"}
	})
	if !errors.Is(err, ErrTransient) || attempts != MaxTransactionAttempts {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
	if IsTransient(context.Canceled) {
		t.Fatal("cancellation classified as a database conflict")
	}
}
