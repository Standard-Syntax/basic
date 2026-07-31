package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RunIntakeRequest struct {
	Idempotency runtime.IdempotencyRequest
	Command     workflow.CreateRun
	Content     json.RawMessage
	BaseCommit  string
}

type RunIntake interface {
	Accept(context.Context, RunIntakeRequest) (*runtime.IdempotencyResult, error)
}

type transactionalWorkflow interface {
	ExecuteRunTx(context.Context, pgx.Tx, workflow.RunCommand) (workflow.CommandResult, error)
}

type IntakeFaultPoint string

const (
	FaultAfterReservation   IntakeFaultPoint = "after_reservation"
	FaultAfterIntakeCAS     IntakeFaultPoint = "after_intake_cas"
	FaultAfterRepositoryCAS IntakeFaultPoint = "after_repository_cas"
	FaultAfterWorkflow      IntakeFaultPoint = "after_workflow"
	FaultAfterBinding       IntakeFaultPoint = "after_binding"
	FaultAfterResponse      IntakeFaultPoint = "after_response"
	FaultIntakeBeforeCommit IntakeFaultPoint = "before_commit"
	FaultIntakeAfterCommit  IntakeFaultPoint = "after_commit"
)

type RunIntakeCoordinator struct {
	pool           *pgxpool.Pool
	ledger         *runtime.Ledger
	workflow       transactionalWorkflow
	artifacts      ArtifactStore
	repositoryRoot string
	inject         func(IntakeFaultPoint) error
}

func NewRunIntakeCoordinator(
	pool *pgxpool.Pool, store transactionalWorkflow, artifacts ArtifactStore,
	repositoryRoot string,
) (*RunIntakeCoordinator, error) {
	if pool == nil || store == nil || artifacts == nil {
		return nil, errors.New("run intake dependencies are required")
	}
	if !filepath.IsAbs(repositoryRoot) || filepath.Clean(repositoryRoot) != repositoryRoot {
		return nil, errors.New("repository root must be clean and absolute")
	}
	return &RunIntakeCoordinator{
		pool: pool, ledger: runtime.NewLedger(pool), workflow: store,
		artifacts: artifacts, repositoryRoot: repositoryRoot,
	}, nil
}

func (c *RunIntakeCoordinator) fault(point IntakeFaultPoint) error {
	if c.inject != nil {
		return c.inject(point)
	}
	return nil
}

func (c *RunIntakeCoordinator) Accept(
	ctx context.Context, request RunIntakeRequest,
) (*runtime.IdempotencyResult, error) {
	if request.Idempotency.Method != http.MethodPost || request.Idempotency.Target != "/v1/runs" ||
		request.Command.Meta.CommandID != request.Idempotency.Key ||
		request.Command.Meta.Actor.ID != request.Idempotency.PrincipalID ||
		request.Command.Meta.Actor.Kind != workflow.ActorHuman ||
		request.Command.Meta.ExpectedRevision != 0 {
		return nil, runtime.ErrConflict
	}
	if err := request.Command.Meta.Validate(); err != nil {
		return nil, err
	}
	reservation, err := c.ledger.BeginIdempotency(ctx, request.Idempotency)
	if err != nil || reservation.Replay {
		return reservation, err
	}
	fence := reservation.FencingToken
	abandon := func(cause error) (*runtime.IdempotencyResult, error) {
		abandonErr := c.ledger.AbandonIdempotency(ctx, request.Idempotency.Key, fence)
		if abandonErr != nil && !errors.Is(abandonErr, runtime.ErrTerminal) {
			return nil, errors.Join(cause, abandonErr)
		}
		return nil, cause
	}
	if err := c.fault(FaultAfterReservation); err != nil {
		return abandon(err)
	}
	intake, err := c.artifacts.Put(ctx, request.Content)
	if err != nil {
		return abandon(err)
	}
	if err := c.fault(FaultAfterIntakeCAS); err != nil {
		return abandon(err)
	}
	snapshot, err := runtime.SnapshotRepository(ctx, c.repositoryRoot, request.BaseCommit)
	if err != nil {
		return abandon(err)
	}
	if snapshot.BaseCommit != request.BaseCommit {
		return abandon(runtime.ErrConflict)
	}
	repositoryBody, err := json.Marshal(snapshot)
	if err != nil {
		return abandon(err)
	}
	repositoryMap, err := c.artifacts.Put(ctx, repositoryBody)
	if err != nil {
		return abandon(err)
	}
	if err := c.fault(FaultAfterRepositoryCAS); err != nil {
		return abandon(err)
	}

	tx, err := c.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return abandon(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	failTx := func(cause error) (*runtime.IdempotencyResult, error) {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			cause = errors.Join(cause, rollbackErr)
		}
		return abandon(cause)
	}
	if err := lockIntakeReservation(ctx, tx, request.Idempotency, fence); err != nil {
		return failTx(err)
	}
	result, err := c.workflow.ExecuteRunTx(ctx, tx, request.Command)
	if err != nil {
		return failTx(err)
	}
	if err := c.fault(FaultAfterWorkflow); err != nil {
		return failTx(err)
	}
	binding := runtime.RunBinding{
		RunID: request.Command.ID, Intake: intake, BaseCommit: request.BaseCommit,
		RepositoryMap: &repositoryMap, CreatedAt: request.Command.Meta.Timestamp,
	}
	if err := runtime.CreateRunBindingTx(ctx, tx, binding); err != nil {
		return failTx(err)
	}
	if err := c.fault(FaultAfterBinding); err != nil {
		return failTx(err)
	}
	response, err := json.Marshal(result)
	if err != nil {
		return failTx(err)
	}
	response, err = completeIntakeReservation(
		ctx, tx, request.Idempotency.Key, fence, http.StatusCreated, response,
	)
	if err != nil {
		return failTx(err)
	}
	if err := c.fault(FaultAfterResponse); err != nil {
		return failTx(err)
	}
	if err := c.fault(FaultIntakeBeforeCommit); err != nil {
		return failTx(err)
	}
	if err := tx.Commit(ctx); err != nil {
		// A commit error can be ambiguous. Leave the fenced reservation intact so
		// a retry either observes the committed response or reclaims after expiry.
		return nil, fmt.Errorf("commit run intake: %w", err)
	}
	if err := c.fault(FaultIntakeAfterCommit); err != nil {
		return nil, err
	}
	return &runtime.IdempotencyResult{
		StatusCode: http.StatusCreated, Response: response, FencingToken: fence,
	}, nil
}

func lockIntakeReservation(
	ctx context.Context, tx pgx.Tx, request runtime.IdempotencyRequest, fence uint64,
) error {
	var method, target, principal, digest string
	var status *int
	var unexpired bool
	var generation uint64
	err := tx.QueryRow(ctx, `SELECT method,target,principal_id::text,request_digest,
		status_code,reservation_expires_at > clock_timestamp(),reservation_generation
		FROM runtime_api_idempotency WHERE idempotency_key=$1 FOR UPDATE`, request.Key).Scan(
		&method, &target, &principal, &digest, &status, &unexpired, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtime.ErrNotFound
	}
	if err != nil {
		return err
	}
	if method != request.Method || target != request.Target || principal != request.PrincipalID ||
		digest != request.RequestDigest {
		return runtime.ErrConflict
	}
	if status != nil {
		return runtime.ErrTerminal
	}
	if generation != fence || !unexpired {
		return runtime.ErrStaleFence
	}
	return nil
}

func completeIntakeReservation(
	ctx context.Context, tx pgx.Tx, key string, fence uint64, status int, response []byte,
) ([]byte, error) {
	var stored []byte
	err := tx.QueryRow(ctx, `UPDATE runtime_api_idempotency
		SET status_code=$3,response=$4,completed_at=now()
		WHERE idempotency_key=$1 AND reservation_generation=$2
		  AND completed_at IS NULL RETURNING response`, key, fence, status, response).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, runtime.ErrStaleFence
	}
	if err != nil {
		return nil, err
	}
	return stored, nil
}
