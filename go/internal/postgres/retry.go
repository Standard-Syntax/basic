// Package postgres provides narrow PostgreSQL failure classification and retry helpers.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	MaxTransactionAttempts = 5
	initialRetryBackoff    = 10 * time.Millisecond
	maximumRetryBackoff    = 250 * time.Millisecond
)

// ErrTransient marks an exhausted serialization or deadlock conflict. Callers
// may safely ask the operator or reconciler to retry the whole operation.
var ErrTransient = errors.New("transient PostgreSQL conflict")

func IsTransient(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) &&
		(postgresError.Code == "40001" || postgresError.Code == "40P01")
}

// RetryTransaction retries a complete transaction boundary. The callback must
// begin, roll back, and commit its own transaction on every invocation.
func RetryTransaction[T any](ctx context.Context, operation func() (T, error)) (T, error) {
	var zero T
	var last error
	for attempt := 0; attempt < MaxTransactionAttempts; attempt++ {
		result, err := operation()
		if err == nil {
			return result, nil
		}
		if !IsTransient(err) {
			return zero, err
		}
		last = err
		if attempt+1 == MaxTransactionAttempts {
			break
		}
		backoff := initialRetryBackoff << attempt
		if backoff > maximumRetryBackoff {
			backoff = maximumRetryBackoff
		}
		jittered := backoff/2 + time.Duration(rand.Int64N(int64(backoff/2)+1))
		timer := time.NewTimer(jittered)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
	return zero, fmt.Errorf("%w: %v", ErrTransient, last)
}
