package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/beta"
	postgresutil "github.com/Standard-Syntax/basic/go/internal/postgres"
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
	FaultAfterPolicyCAS     IntakeFaultPoint = "after_policy_cas"
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
	policy         beta.Policy
	inject         func(IntakeFaultPoint) error
	renewEvery     time.Duration
	renew          func(context.Context, string, uint64, time.Duration) error
}

const (
	intakeCleanupTimeout  = 10 * time.Second
	idempotencyRenewEvery = 10 * time.Second
)

type stagedRunIntake struct {
	intake        workflow.ArtifactRef
	repositoryMap workflow.ArtifactRef
	policy        workflow.ArtifactRef
}

func NewRunIntakeCoordinator(
	pool *pgxpool.Pool, store transactionalWorkflow, artifacts ArtifactStore,
	repositoryRoot string, configuredPolicy ...beta.Policy,
) (*RunIntakeCoordinator, error) {
	if pool == nil || store == nil || artifacts == nil {
		return nil, errors.New("run intake dependencies are required")
	}
	if !filepath.IsAbs(repositoryRoot) || filepath.Clean(repositoryRoot) != repositoryRoot {
		return nil, errors.New("repository root must be clean and absolute")
	}
	if len(configuredPolicy) != 1 || configuredPolicy[0].Repository.Root != repositoryRoot {
		return nil, errors.New("one matching beta policy is required")
	}
	if err := configuredPolicy[0].Validate(); err != nil {
		return nil, err
	}
	ledger := runtime.NewLedger(pool)
	return &RunIntakeCoordinator{
		pool: pool, ledger: ledger, workflow: store,
		artifacts: artifacts, repositoryRoot: repositoryRoot, policy: configuredPolicy[0],
		renewEvery: idempotencyRenewEvery,
		renew:      ledger.RenewIdempotency,
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
) (result *runtime.IdempotencyResult, err error) {
	if err := validateRunIntakeRequest(request); err != nil {
		return nil, err
	}
	if request.Idempotency.ReservationTTL <= 0 {
		request.Idempotency.ReservationTTL = 30 * time.Second
	}
	reservation, err := c.ledger.BeginIdempotency(ctx, request.Idempotency)
	if err != nil || reservation.Replay {
		return reservation, err
	}
	fence := reservation.FencingToken
	committed := false
	workCtx, cancelWork := context.WithCancel(ctx)
	renewed := make(chan error, 1)
	go c.renewReservation(workCtx, cancelWork, request.Idempotency, fence, renewed)
	defer func() {
		cancelWork()
		renewErr := <-renewed
		if !committed && renewErr != nil && (err == nil || errors.Is(err, context.Canceled)) {
			result, err = nil, renewErr
		}
	}()
	if err := c.fault(FaultAfterReservation); err != nil {
		return nil, c.abandonReservation(ctx, request.Idempotency.Key, fence, err)
	}
	staged, err := c.stageArtifacts(workCtx, request)
	if err != nil {
		return nil, c.abandonReservation(ctx, request.Idempotency.Key, fence, err)
	}
	response, commitAttempted, err := c.commitIntake(workCtx, request, fence, staged)
	if err != nil {
		if commitAttempted {
			return nil, err
		}
		return nil, c.abandonReservation(ctx, request.Idempotency.Key, fence, err)
	}
	committed = true
	if err := c.fault(FaultIntakeAfterCommit); err != nil {
		return nil, err
	}
	return &runtime.IdempotencyResult{
		StatusCode: http.StatusCreated, Response: response, FencingToken: fence,
	}, nil
}

func (c *RunIntakeCoordinator) renewReservation(
	ctx context.Context, cancel context.CancelFunc, request runtime.IdempotencyRequest,
	fence uint64, done chan<- error,
) {
	ticker := time.NewTicker(c.renewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-ticker.C:
			renewCtx, renewCancel := detachedCleanupContext(ctx)
			err := c.renew(
				renewCtx, request.Key, fence, request.ReservationTTL,
			)
			renewCancel()
			if err != nil {
				cancel()
				done <- err
				return
			}
		}
	}
}

func validateRunIntakeRequest(request RunIntakeRequest) error {
	if request.Idempotency.Method != http.MethodPost || request.Idempotency.Target != "/v1/runs" ||
		request.Command.Meta.CommandID != request.Idempotency.Key ||
		request.Command.Meta.Actor.ID != request.Idempotency.PrincipalID ||
		request.Command.Meta.Actor.Kind != workflow.ActorHuman ||
		request.Command.Meta.ExpectedRevision != 0 {
		return runtime.ErrConflict
	}
	return request.Command.Meta.Validate()
}

func (c *RunIntakeCoordinator) stageArtifacts(
	ctx context.Context, request RunIntakeRequest,
) (stagedRunIntake, error) {
	if request.BaseCommit != c.policy.Repository.BaseCommit {
		return stagedRunIntake{}, runtime.ErrConflict
	}
	if err := beta.VerifyRemoteBase(ctx, c.policy); err != nil {
		return stagedRunIntake{}, err
	}
	intake, err := c.artifacts.Put(ctx, request.Content)
	if err != nil {
		return stagedRunIntake{}, err
	}
	if err := c.fault(FaultAfterIntakeCAS); err != nil {
		return stagedRunIntake{}, err
	}
	snapshot, err := runtime.SnapshotRepository(ctx, c.repositoryRoot, request.BaseCommit)
	if err != nil {
		return stagedRunIntake{}, err
	}
	if snapshot.BaseCommit != request.BaseCommit {
		return stagedRunIntake{}, runtime.ErrConflict
	}
	repositoryBody, err := json.Marshal(snapshot)
	if err != nil {
		return stagedRunIntake{}, err
	}
	repositoryMap, err := c.artifacts.Put(ctx, repositoryBody)
	if err != nil {
		return stagedRunIntake{}, err
	}
	if err := c.fault(FaultAfterRepositoryCAS); err != nil {
		return stagedRunIntake{}, err
	}
	policyBody, _, err := c.policy.Canonical()
	if err != nil {
		return stagedRunIntake{}, err
	}
	policy, err := c.artifacts.Put(ctx, policyBody)
	if err != nil {
		return stagedRunIntake{}, err
	}
	if err := c.fault(FaultAfterPolicyCAS); err != nil {
		return stagedRunIntake{}, err
	}
	return stagedRunIntake{intake: intake, repositoryMap: repositoryMap, policy: policy}, nil
}

func (c *RunIntakeCoordinator) commitIntake(
	ctx context.Context, request RunIntakeRequest, fence uint64, staged stagedRunIntake,
) (response []byte, commitAttempted bool, err error) {
	type result struct{ response []byte }
	value, err := postgresutil.RetryTransaction(ctx, func() (result, error) {
		body, attempted, transactionErr := c.commitIntakeOnce(ctx, request, fence, staged)
		commitAttempted = commitAttempted || attempted
		return result{response: body}, transactionErr
	})
	return value.response, commitAttempted, err
}

func (c *RunIntakeCoordinator) commitIntakeOnce(
	ctx context.Context, request RunIntakeRequest, fence uint64, staged stagedRunIntake,
) (response []byte, commitAttempted bool, err error) {
	tx, err := c.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, false, err
	}
	defer func() {
		err = rollbackIntake(ctx, tx, commitAttempted, err)
	}()
	if err := lockIntakeReservation(ctx, tx, request.Idempotency, fence); err != nil {
		return nil, false, err
	}
	result, err := c.workflow.ExecuteRunTx(ctx, tx, request.Command)
	if err != nil {
		return nil, false, err
	}
	if err := c.fault(FaultAfterWorkflow); err != nil {
		return nil, false, err
	}
	binding := runtime.RunBinding{
		RunID: request.Command.ID, Intake: staged.intake, BaseCommit: request.BaseCommit,
		RepositoryMap: &staged.repositoryMap, Policy: &staged.policy,
		ExecutionImageDigest:    c.policy.Images.Execution,
		VerificationImageDigest: c.policy.Images.Verification,
		CreatedAt:               request.Command.Meta.Timestamp,
	}
	if err := runtime.CreateRunBindingTx(ctx, tx, binding); err != nil {
		return nil, false, err
	}
	if err := c.fault(FaultAfterBinding); err != nil {
		return nil, false, err
	}
	response, err = json.Marshal(result)
	if err != nil {
		return nil, false, err
	}
	response, err = completeIntakeReservation(
		ctx, tx, request.Idempotency.Key, fence, http.StatusCreated, response,
	)
	if err != nil {
		return nil, false, err
	}
	if err := c.fault(FaultAfterResponse); err != nil {
		return nil, false, err
	}
	if err := c.fault(FaultIntakeBeforeCommit); err != nil {
		return nil, false, err
	}
	commitAttempted = true
	if err = tx.Commit(ctx); err != nil {
		// A commit error can be ambiguous. Leave the fenced reservation intact so
		// a retry either observes the committed response or reclaims after expiry.
		return nil, true, fmt.Errorf("commit run intake: %w", err)
	}
	return response, true, nil
}

func rollbackIntake(ctx context.Context, tx pgx.Tx, commitAttempted bool, cause error) error {
	if cause == nil || commitAttempted {
		return cause
	}
	cleanupCtx, cancel := detachedCleanupContext(ctx)
	defer cancel()
	rollbackErr := tx.Rollback(cleanupCtx)
	if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
		return errors.Join(cause, rollbackErr)
	}
	return cause
}

func (c *RunIntakeCoordinator) abandonReservation(
	ctx context.Context, key string, fence uint64, cause error,
) error {
	cleanupCtx, cancel := detachedCleanupContext(ctx)
	defer cancel()
	if abandonErr := c.ledger.AbandonIdempotency(cleanupCtx, key, fence); abandonErr != nil &&
		!errors.Is(abandonErr, runtime.ErrTerminal) {
		return errors.Join(cause, abandonErr)
	}
	return cause
}

func detachedCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), intakeCleanupTimeout)
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
