package verification

import (
	"bytes"
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
var verificationMigrationFiles embed.FS

func MigrationSource() migration.Source {
	return migration.Source{Files: verificationMigrationFiles, Directory: "migrations"}
}

func Migrate(ctx context.Context, connectionString string) error {
	return migration.Apply(
		ctx, connectionString, verificationMigrationFiles, "migrations",
	)
}

type PostgresVerificationLedger struct {
	pool *pgxpool.Pool
}

func NewPostgresVerificationLedger(pool *pgxpool.Pool) *PostgresVerificationLedger {
	return &PostgresVerificationLedger{pool: pool}
}

func (r *PostgresVerificationLedger) Begin(
	ctx context.Context, start VerificationStart,
) (VerificationHandle, error) {
	if start.ReservationTTL <= 0 {
		return nil, errors.New("positive reservation TTL is required")
	}
	seconds := start.ReservationTTL.Seconds()
	for {
		owner := uuid.NewString()
		handle, wait, err := r.beginAttempt(ctx, start, owner, seconds)
		if err != nil {
			return nil, err
		}
		if handle != nil {
			return handle, nil
		}
		if !wait {
			continue
		}
		if err := waitForReservation(ctx); err != nil {
			return nil, err
		}
	}
}

type reservationRecord struct {
	digest       string
	state        string
	owner        string
	evidenceJSON []byte
	resultJSON   []byte
}

func (r *PostgresVerificationLedger) beginAttempt(
	ctx context.Context, start VerificationStart, owner string, seconds float64,
) (VerificationHandle, bool, error) {
	tag, err := r.pool.Exec(ctx, `INSERT INTO verification_ledger (
		verification_id,request_digest,owner_id,reserved_until,state,verification_timestamp
	) VALUES ($1,$2,$3,clock_timestamp()+make_interval(secs => $4),'reserved',$5)
	ON CONFLICT (verification_id) DO NOTHING`,
		start.VerificationID, start.RequestDigest, owner, seconds, start.Timestamp,
	)
	if err != nil {
		return nil, false, fmt.Errorf("reserve verification: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return r.ownedHandle(start.VerificationID, start.RequestDigest, owner), false, nil
	}
	record, found, err := r.readReservation(ctx, start.VerificationID)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	handle, err := r.handleExistingReservation(record, start)
	if handle != nil || err != nil {
		return handle, false, err
	}
	recovered, err := r.recoverReservation(ctx, start, owner, seconds)
	if err != nil {
		return nil, false, err
	}
	if recovered {
		return r.ownedHandle(start.VerificationID, start.RequestDigest, owner), false, nil
	}
	return nil, true, nil
}

func (r *PostgresVerificationLedger) readReservation(
	ctx context.Context, verificationID string,
) (reservationRecord, bool, error) {
	var record reservationRecord
	err := r.pool.QueryRow(ctx, `SELECT request_digest,state,owner_id,evidence_json,result_json
		FROM verification_ledger WHERE verification_id=$1`, verificationID,
	).Scan(
		&record.digest, &record.state, &record.owner,
		&record.evidenceJSON, &record.resultJSON,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return reservationRecord{}, false, nil
	}
	if err != nil {
		return reservationRecord{}, false, fmt.Errorf("read verification reservation: %w", err)
	}
	return record, true, nil
}

func (r *PostgresVerificationLedger) handleExistingReservation(
	record reservationRecord, start VerificationStart,
) (VerificationHandle, error) {
	if record.digest != start.RequestDigest {
		return nil, ErrVerificationConflict
	}
	switch record.state {
	case "completed":
		var result Result
		if err := json.Unmarshal(record.resultJSON, &result); err != nil {
			return nil, fmt.Errorf("decode completed verification: %w", err)
		}
		return &postgresVerificationHandle{replay: &result}, nil
	case "evidence_ready":
		var evidence VerificationEvidence
		if err := json.Unmarshal(record.evidenceJSON, &evidence); err != nil {
			return nil, fmt.Errorf("decode verification evidence: %w", err)
		}
		return &postgresVerificationHandle{
			ledger: r, verificationID: start.VerificationID,
			digest: record.digest, owner: record.owner, evidence: &evidence,
		}, nil
	default:
		return nil, nil
	}
}

func (r *PostgresVerificationLedger) recoverReservation(
	ctx context.Context, start VerificationStart, owner string, seconds float64,
) (bool, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE verification_ledger
		SET owner_id=$2,reserved_until=clock_timestamp()+make_interval(secs => $3)
		WHERE verification_id=$1 AND request_digest=$4 AND state='reserved'
		  AND reserved_until <= clock_timestamp()`,
		start.VerificationID, owner, seconds, start.RequestDigest,
	)
	if err != nil {
		return false, fmt.Errorf("recover verification reservation: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *PostgresVerificationLedger) ownedHandle(
	verificationID string, digest string, owner string,
) VerificationHandle {
	return &postgresVerificationHandle{
		ledger: r, verificationID: verificationID, digest: digest, owner: owner,
	}
}

func waitForReservation(ctx context.Context) error {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type postgresVerificationHandle struct {
	ledger         *PostgresVerificationLedger
	verificationID string
	digest         string
	owner          string
	replay         *Result
	evidence       *VerificationEvidence
}

func (h *postgresVerificationHandle) Replay() (Result, bool) {
	if h.replay == nil {
		return Result{}, false
	}
	return cloneResult(*h.replay), true
}

func (h *postgresVerificationHandle) Evidence() (VerificationEvidence, bool) {
	if h.evidence == nil {
		return VerificationEvidence{}, false
	}
	return cloneEvidence(*h.evidence), true
}

func (h *postgresVerificationHandle) SaveEvidence(
	ctx context.Context, evidence VerificationEvidence,
) error {
	body, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("encode verification evidence: %w", err)
	}
	tag, err := h.ledger.pool.Exec(ctx, `UPDATE verification_ledger
		SET state='evidence_ready',evidence_json=$4,evidence_ready_at=clock_timestamp()
		WHERE verification_id=$1 AND request_digest=$2 AND owner_id=$3 AND state='reserved'`,
		h.verificationID, h.digest, h.owner, body,
	)
	if err != nil {
		return fmt.Errorf("checkpoint verification evidence: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrVerificationConflict
	}
	stored := cloneEvidence(evidence)
	h.evidence = &stored
	return nil
}

func (h *postgresVerificationHandle) FinalTransitionTime(
	ctx context.Context, value time.Time,
) (time.Time, error) {
	var stored time.Time
	err := h.ledger.pool.QueryRow(ctx, `UPDATE verification_ledger
		SET final_transition_at=COALESCE(final_transition_at,$4)
		WHERE verification_id=$1 AND request_digest=$2 AND owner_id=$3
		  AND state='evidence_ready'
		RETURNING final_transition_at`,
		h.verificationID, h.digest, h.owner, value.UTC(),
	).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrVerificationConflict
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("record verification transition time: %w", err)
	}
	return stored, nil
}

func (h *postgresVerificationHandle) Complete(ctx context.Context, result Result) error {
	body, err := resultBytes(result)
	if err != nil {
		return fmt.Errorf("encode verification result: %w", err)
	}
	tag, err := h.ledger.pool.Exec(ctx, `UPDATE verification_ledger
		SET state='completed',result_json=$4,completed_at=clock_timestamp()
		WHERE verification_id=$1 AND request_digest=$2 AND owner_id=$3
		  AND state='evidence_ready' AND final_transition_at IS NOT NULL`,
		h.verificationID, h.digest, h.owner, body,
	)
	if err != nil {
		return fmt.Errorf("complete verification ledger: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrVerificationConflict
	}
	return nil
}

func resultBytes(value Result) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}
