package controlapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/orchestration"
	postgresutil "github.com/Standard-Syntax/basic/go/internal/postgres"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type TaskGraphApprovalRequest struct {
	Command  workflow.ApproveTaskGraph
	Graph    workflow.ArtifactRef
	Task     runtime.TaskBinding
	StartJob runtime.Job
}

type TaskGraphApproval interface {
	ApproveTaskGraph(context.Context, TaskGraphApprovalRequest) (workflow.CommandResult, error)
}

type TaskGraphApprovalFaultPoint string

const (
	FaultTaskGraphAfterWorkflow TaskGraphApprovalFaultPoint = "after_workflow"
	FaultTaskGraphAfterBinding  TaskGraphApprovalFaultPoint = "after_binding"
	FaultTaskGraphAfterEnqueue  TaskGraphApprovalFaultPoint = "after_enqueue"
	taskGraphCleanupTimeout                                 = 10 * time.Second
)

type TaskGraphApprovalCoordinator struct {
	pool     *pgxpool.Pool
	workflow transactionalWorkflow
	inject   func(TaskGraphApprovalFaultPoint) error
}

func NewTaskGraphApprovalCoordinator(
	pool *pgxpool.Pool, store transactionalWorkflow,
) (*TaskGraphApprovalCoordinator, error) {
	if pool == nil {
		return nil, errors.New("task graph approval pool is required")
	}
	if store == nil {
		return nil, errors.New("task graph approval workflow store is required")
	}
	return &TaskGraphApprovalCoordinator{pool: pool, workflow: store}, nil
}

func (c *TaskGraphApprovalCoordinator) ApproveTaskGraph(
	ctx context.Context, request TaskGraphApprovalRequest,
) (workflow.CommandResult, error) {
	if err := validateTaskGraphApproval(request); err != nil {
		return workflow.CommandResult{}, err
	}
	return postgresutil.RetryTransaction(ctx, func() (workflow.CommandResult, error) {
		return c.approveOnce(ctx, request)
	})
}

func (c *TaskGraphApprovalCoordinator) approveOnce(
	ctx context.Context, request TaskGraphApprovalRequest,
) (result workflow.CommandResult, err error) {
	tx, err := c.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return workflow.CommandResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			err = rollbackTaskGraphApproval(ctx, tx, err)
		}
	}()
	result, err = c.workflow.ExecuteRunTx(ctx, tx, request.Command)
	if err != nil {
		return workflow.CommandResult{}, err
	}
	if err = c.fault(FaultTaskGraphAfterWorkflow); err != nil {
		return workflow.CommandResult{}, err
	}
	if err = runtime.CheckpointTaskGraphTx(
		ctx, tx, request.Command.ID, request.Graph, request.Task,
	); err != nil {
		return workflow.CommandResult{}, err
	}
	if err = c.fault(FaultTaskGraphAfterBinding); err != nil {
		return workflow.CommandResult{}, err
	}
	if err = runtime.EnqueueTx(ctx, tx, request.StartJob); err != nil {
		return workflow.CommandResult{}, err
	}
	if err = c.fault(FaultTaskGraphAfterEnqueue); err != nil {
		return workflow.CommandResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return workflow.CommandResult{}, fmt.Errorf("commit task graph approval: %w", err)
	}
	committed = true
	return result, nil
}

func (c *TaskGraphApprovalCoordinator) fault(point TaskGraphApprovalFaultPoint) error {
	if c.inject != nil {
		return c.inject(point)
	}
	return nil
}

func validateTaskGraphApproval(request TaskGraphApprovalRequest) error {
	if len(request.Command.Tasks) != 1 || len(request.Command.Dependencies) != 0 ||
		!request.Command.TaskGraph.Equal(request.Graph) {
		return workflow.ErrInvalid
	}
	definition := request.Command.Tasks[0]
	jobTask := request.StartJob.TaskID
	if request.Task.RunID != request.Command.ID || request.Task.TaskID != definition.ID ||
		jobTask == nil || *jobTask != definition.ID || request.StartJob.RunID != request.Command.ID ||
		request.StartJob.Attempt != 1 || request.StartJob.Stage != orchestration.StageStart ||
		request.StartJob.AvailableAt.UTC() != request.Command.Meta.Timestamp.UTC() ||
		request.StartJob.ID != orchestration.StableID(
			request.Command.ID, definition.ID, "1", orchestration.StageStart, "job",
		) {
		return workflow.ErrInvalid
	}
	return nil
}

func rollbackTaskGraphApproval(ctx context.Context, tx pgx.Tx, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), taskGraphCleanupTimeout)
	defer cancel()
	rollbackErr := tx.Rollback(cleanupCtx)
	if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
		return errors.Join(cause, rollbackErr)
	}
	return cause
}
