// Package runtime owns durable API and reconciler checkpoints.
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrConflict   = errors.New("runtime identity conflicts with existing content")
	ErrInProgress = errors.New("runtime request is still processing")
	ErrNotFound   = errors.New("runtime record not found")
	ErrStaleFence = errors.New("stale runtime fencing token")
	ErrTerminal   = errors.New("runtime job is terminal")
)

type Job struct {
	ID           string
	RunID        string
	TaskID       *string
	Attempt      uint32
	Stage        string
	State        string
	AvailableAt  time.Time
	ClaimOwner   *string
	ClaimExpires *time.Time
	FencingToken uint64
	RetryCount   uint32
	Result       *workflow.ArtifactRef
	Failure      *workflow.ArtifactRef
}

type IdempotencyRequest struct {
	Key, Method, Target, PrincipalID, RequestDigest string
}

type IdempotencyResult struct {
	StatusCode int
	Response   json.RawMessage
	Replay     bool
}

type Ledger struct{ pool *pgxpool.Pool }

func NewLedger(pool *pgxpool.Pool) *Ledger { return &Ledger{pool: pool} }

func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (l *Ledger) BeginIdempotency(ctx context.Context, request IdempotencyRequest) (*IdempotencyResult, error) {
	if _, err := uuid.Parse(request.Key); err != nil {
		return nil, fmt.Errorf("%w: idempotency key", ErrConflict)
	}
	now := time.Now().UTC()
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `INSERT INTO runtime_api_idempotency
		(idempotency_key,method,target,principal_id,request_digest,reservation_expires_at)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`,
		request.Key, request.Method, request.Target, request.PrincipalID,
		request.RequestDigest, now.Add(30*time.Second))
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		return nil, tx.Commit(ctx)
	}
	var method, target, principal, digest string
	var status *int
	var response []byte
	var reservedUntil time.Time
	err = tx.QueryRow(ctx, `SELECT method,target,principal_id::text,request_digest,
		status_code,response,reservation_expires_at FROM runtime_api_idempotency
		WHERE idempotency_key=$1 FOR UPDATE`, request.Key).Scan(
		&method, &target, &principal, &digest, &status, &response, &reservedUntil)
	if err != nil {
		return nil, err
	}
	if method != request.Method || target != request.Target ||
		principal != request.PrincipalID || digest != request.RequestDigest {
		return nil, ErrConflict
	}
	if status == nil {
		if reservedUntil.After(now) {
			return nil, ErrInProgress
		}
		_, err = tx.Exec(ctx, `UPDATE runtime_api_idempotency
			SET reservation_expires_at=$2 WHERE idempotency_key=$1`,
			request.Key, now.Add(30*time.Second))
		if err != nil {
			return nil, err
		}
		return nil, tx.Commit(ctx)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &IdempotencyResult{StatusCode: *status, Response: response, Replay: true}, nil
}

func (l *Ledger) CompleteIdempotency(
	ctx context.Context, key string, status int, response json.RawMessage,
) error {
	tag, err := l.pool.Exec(ctx, `UPDATE runtime_api_idempotency
		SET status_code=$2,response=$3,completed_at=now()
		WHERE idempotency_key=$1 AND completed_at IS NULL`, key, status, response)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		var completedAt *time.Time
		err := l.pool.QueryRow(ctx, `SELECT completed_at FROM runtime_api_idempotency
			WHERE idempotency_key=$1`, key).Scan(&completedAt)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		case completedAt != nil:
			return ErrTerminal
		default:
			return ErrConflict
		}
	}
	return nil
}

func (l *Ledger) Enqueue(ctx context.Context, job Job) error {
	var task any
	if job.TaskID != nil {
		task = *job.TaskID
	}
	tag, err := l.pool.Exec(ctx, `INSERT INTO runtime_stage_jobs
		(job_id,run_id,task_id,attempt,stage,state,available_at,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,'READY',$6,$6,$6) ON CONFLICT DO NOTHING`,
		job.ID, job.RunID, task, job.Attempt, job.Stage, job.AvailableAt.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var id string
	err = l.pool.QueryRow(ctx, `SELECT job_id::text FROM runtime_stage_jobs
		WHERE run_id=$1 AND task_id IS NOT DISTINCT FROM $2 AND attempt=$3 AND stage=$4`,
		job.RunID, task, job.Attempt, job.Stage).Scan(&id)
	if err != nil {
		return err
	}
	if id != job.ID {
		return ErrConflict
	}
	return nil
}

func (l *Ledger) Claim(ctx context.Context, owner string, now time.Time, ttl time.Duration) (Job, bool, error) {
	tx, err := l.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Job{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var job Job
	var taskID, claimOwner, resultURI, resultDigest, failureURI, failureDigest *string
	var claimExpires *time.Time
	err = tx.QueryRow(ctx, `SELECT job_id::text,run_id::text,task_id::text,attempt,stage,
		state,available_at,claim_owner::text,claim_expires_at,fencing_token,retry_count,
		result_uri,result_digest,failure_uri,failure_digest
		FROM runtime_stage_jobs
		WHERE state IN ('READY','RETRY','CLAIMED') AND available_at <= $1
		  AND (state <> 'CLAIMED' OR claim_expires_at <= $1)
		ORDER BY available_at,job_id FOR UPDATE SKIP LOCKED LIMIT 1`, now.UTC()).Scan(
		&job.ID, &job.RunID, &taskID, &job.Attempt, &job.Stage, &job.State,
		&job.AvailableAt, &claimOwner, &claimExpires, &job.FencingToken,
		&job.RetryCount, &resultURI, &resultDigest, &failureURI, &failureDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	job.TaskID = taskID
	if resultURI != nil && resultDigest != nil {
		job.Result = &workflow.ArtifactRef{URI: *resultURI, Digest: *resultDigest}
	}
	if failureURI != nil && failureDigest != nil {
		job.Failure = &workflow.ArtifactRef{URI: *failureURI, Digest: *failureDigest}
	}
	expires := now.UTC().Add(ttl)
	job.FencingToken++
	tag, err := tx.Exec(ctx, `UPDATE runtime_stage_jobs SET state='CLAIMED',
		claim_owner=$2,claim_expires_at=$3,fencing_token=$4,updated_at=$5 WHERE job_id=$1`,
		job.ID, owner, expires, job.FencingToken, now.UTC())
	if err != nil || tag.RowsAffected() != 1 {
		return Job{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, false, err
	}
	job.State, job.ClaimOwner, job.ClaimExpires = "CLAIMED", &owner, &expires
	return job, true, nil
}

func (l *Ledger) Complete(
	ctx context.Context, jobID, owner string, fence uint64, result workflow.ArtifactRef, now time.Time,
) error {
	if err := result.Validate(); err != nil {
		return err
	}
	tag, err := l.pool.Exec(ctx, `UPDATE runtime_stage_jobs SET state='COMPLETED',
		result_uri=$4,result_digest=$5,claim_owner=NULL,claim_expires_at=NULL,updated_at=$6
		WHERE job_id=$1 AND state='CLAIMED' AND claim_owner=$2 AND fencing_token=$3`,
		jobID, owner, fence, result.URI, result.Digest, now.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return l.classifyJobUpdate(ctx, jobID)
	}
	return nil
}

func (l *Ledger) CompleteAndEnqueue(
	ctx context.Context,
	jobID, owner string,
	fence uint64,
	result workflow.ArtifactRef,
	next *Job,
	now time.Time,
) error {
	if err := result.Validate(); err != nil {
		return err
	}
	tx, err := l.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE runtime_stage_jobs SET state='COMPLETED',
		result_uri=$4,result_digest=$5,claim_owner=NULL,claim_expires_at=NULL,updated_at=$6
		WHERE job_id=$1 AND state='CLAIMED' AND claim_owner=$2 AND fencing_token=$3`,
		jobID, owner, fence, result.URI, result.Digest, now.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return classifyJobUpdate(ctx, tx, jobID)
	}
	if next != nil {
		if err := enqueueTx(ctx, tx, *next); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (l *Ledger) Retry(
	ctx context.Context, jobID, owner string, fence uint64, available time.Time,
) error {
	tag, err := l.pool.Exec(ctx, `UPDATE runtime_stage_jobs SET state='RETRY',
		retry_count=retry_count+1,available_at=$4,claim_owner=NULL,
		claim_expires_at=NULL,updated_at=now()
		WHERE job_id=$1 AND state='CLAIMED' AND claim_owner=$2 AND fencing_token=$3`,
		jobID, owner, fence, available.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return l.classifyJobUpdate(ctx, jobID)
	}
	return nil
}

func (l *Ledger) Fail(
	ctx context.Context, jobID, owner string, fence uint64, failure workflow.ArtifactRef, now time.Time,
) error {
	if err := failure.Validate(); err != nil {
		return err
	}
	tag, err := l.pool.Exec(ctx, `UPDATE runtime_stage_jobs SET state='FAILED',
		failure_uri=$4,failure_digest=$5,claim_owner=NULL,claim_expires_at=NULL,updated_at=$6
		WHERE job_id=$1 AND state='CLAIMED' AND claim_owner=$2 AND fencing_token=$3`,
		jobID, owner, fence, failure.URI, failure.Digest, now.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return l.classifyJobUpdate(ctx, jobID)
	}
	return nil
}

func (l *Ledger) CancelRun(ctx context.Context, runID string, now time.Time) error {
	_, err := l.pool.Exec(ctx, `UPDATE runtime_stage_jobs SET state='CANCELLED',
		claim_owner=NULL,claim_expires_at=NULL,updated_at=$2
		WHERE run_id=$1 AND state IN ('READY','RETRY','CLAIMED')`, runID, now.UTC())
	return err
}

func (l *Ledger) classifyJobUpdate(ctx context.Context, jobID string) error {
	return classifyJobUpdate(ctx, l.pool, jobID)
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func classifyJobUpdate(ctx context.Context, queryer queryRower, jobID string) error {
	var state string
	err := queryer.QueryRow(ctx, `SELECT state FROM runtime_stage_jobs WHERE job_id=$1`, jobID).Scan(&state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return err
	case state == "COMPLETED" || state == "FAILED" || state == "CANCELLED":
		return ErrTerminal
	default:
		return ErrStaleFence
	}
}

func enqueueTx(ctx context.Context, tx pgx.Tx, job Job) error {
	var task any
	if job.TaskID != nil {
		task = *job.TaskID
	}
	tag, err := tx.Exec(ctx, `INSERT INTO runtime_stage_jobs
		(job_id,run_id,task_id,attempt,stage,state,available_at,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,'READY',$6,$6,$6) ON CONFLICT DO NOTHING`,
		job.ID, job.RunID, task, job.Attempt, job.Stage, job.AvailableAt.UTC())
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT job_id::text FROM runtime_stage_jobs
		WHERE run_id=$1 AND task_id IS NOT DISTINCT FROM $2 AND attempt=$3 AND stage=$4`,
		job.RunID, task, job.Attempt, job.Stage).Scan(&id)
	if err != nil {
		return err
	}
	if id != job.ID {
		return ErrConflict
	}
	return nil
}
