package execution

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/migration"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var executionMigrationFiles embed.FS

func Migrate(ctx context.Context, connectionString string) error {
	return migration.Apply(
		ctx, connectionString, executionMigrationFiles, "migrations",
	)
}

type PostgresExecutionLedger struct {
	pool *pgxpool.Pool
}

func NewPostgresExecutionLedger(pool *pgxpool.Pool) *PostgresExecutionLedger {
	return &PostgresExecutionLedger{pool: pool}
}

func (r *PostgresExecutionLedger) Begin(
	ctx context.Context, start ExecutionStart,
) (ExecutionHandle, error) {
	if start.ReservationTTL <= 0 {
		return nil, errors.New("positive reservation TTL is required")
	}
	seconds := start.ReservationTTL.Seconds()
	for {
		owner := uuid.NewString()
		tag, err := r.pool.Exec(ctx, `INSERT INTO execution_ledger (
			execution_id,request_digest,owner_id,reserved_until,state,execution_timestamp
		) VALUES ($1,$2,$3,clock_timestamp()+make_interval(secs => $4),'reserved',$5)
		ON CONFLICT (execution_id) DO NOTHING`,
			start.ExecutionID, start.RequestDigest, owner, seconds, start.Timestamp,
		)
		if err != nil {
			return nil, fmt.Errorf("reserve execution: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return &postgresExecutionHandle{
				ledger: r, executionID: start.ExecutionID,
				digest: start.RequestDigest, owner: owner,
			}, nil
		}
		var digest, state string
		var resultJSON []byte
		err = r.pool.QueryRow(ctx, `SELECT request_digest,state,result_json
			FROM execution_ledger WHERE execution_id=$1`, start.ExecutionID,
		).Scan(&digest, &state, &resultJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read execution reservation: %w", err)
		}
		if digest != start.RequestDigest {
			return nil, ErrExecutionConflict
		}
		if state == "completed" {
			var result Result
			if err := json.Unmarshal(resultJSON, &result); err != nil {
				return nil, fmt.Errorf("decode completed execution: %w", err)
			}
			return &postgresExecutionHandle{replay: &result}, nil
		}
		tag, err = r.pool.Exec(ctx, `UPDATE execution_ledger
			SET owner_id=$2,reserved_until=clock_timestamp()+make_interval(secs => $3)
			WHERE execution_id=$1 AND request_digest=$4 AND state='reserved'
			  AND reserved_until <= clock_timestamp()`,
			start.ExecutionID, owner, seconds, start.RequestDigest,
		)
		if err != nil {
			return nil, fmt.Errorf("recover execution reservation: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return &postgresExecutionHandle{
				ledger: r, executionID: start.ExecutionID,
				digest: start.RequestDigest, owner: owner,
			}, nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

type postgresExecutionHandle struct {
	ledger      *PostgresExecutionLedger
	executionID string
	digest      string
	owner       string
	replay      *Result
}

func (h *postgresExecutionHandle) Replay() (Result, bool) {
	if h.replay == nil {
		return Result{}, false
	}
	return cloneResult(*h.replay), true
}

func (h *postgresExecutionHandle) FinalTransitionTime(
	ctx context.Context, value time.Time,
) (time.Time, error) {
	var stored time.Time
	err := h.ledger.pool.QueryRow(ctx, `UPDATE execution_ledger
		SET final_transition_at=COALESCE(final_transition_at,$4)
		WHERE execution_id=$1 AND request_digest=$2 AND owner_id=$3 AND state='reserved'
		RETURNING final_transition_at`,
		h.executionID, h.digest, h.owner, value.UTC(),
	).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrExecutionConflict
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("record final transition time: %w", err)
	}
	return stored, nil
}

func (h *postgresExecutionHandle) Complete(ctx context.Context, result Result) error {
	body, err := resultBytes(result)
	if err != nil {
		return fmt.Errorf("encode execution result: %w", err)
	}
	tag, err := h.ledger.pool.Exec(ctx, `UPDATE execution_ledger
		SET state='completed',result_json=$4,completed_at=clock_timestamp()
		WHERE execution_id=$1 AND request_digest=$2 AND owner_id=$3 AND state='reserved'`,
		h.executionID, h.digest, h.owner, body,
	)
	if err != nil {
		return fmt.Errorf("complete execution ledger: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrExecutionConflict
	}
	return nil
}
