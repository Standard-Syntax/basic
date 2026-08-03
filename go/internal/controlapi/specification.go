package controlapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/Standard-Syntax/basic/go/internal/orchestration"
	postgresutil "github.com/Standard-Syntax/basic/go/internal/postgres"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type SpecificationApprovalRequest struct {
	Command     workflow.ApproveSpecification
	PlanningJob runtime.Job
}

type SpecificationApproval interface {
	ApproveSpecification(context.Context, SpecificationApprovalRequest) (workflow.CommandResult, error)
}

type SpecificationApprovalFaultPoint string

const (
	FaultSpecificationAfterWorkflow SpecificationApprovalFaultPoint = "after_workflow"
	FaultSpecificationAfterBinding  SpecificationApprovalFaultPoint = "after_binding"
	FaultSpecificationAfterEnqueue  SpecificationApprovalFaultPoint = "after_enqueue"
)

type SpecificationApprovalCoordinator struct {
	pool     *pgxpool.Pool
	workflow transactionalWorkflow
	inject   func(SpecificationApprovalFaultPoint) error
}

func NewSpecificationApprovalCoordinator(
	pool *pgxpool.Pool, store transactionalWorkflow,
) (*SpecificationApprovalCoordinator, error) {
	if pool == nil || store == nil {
		return nil, errors.New("specification approval dependencies are required")
	}
	return &SpecificationApprovalCoordinator{pool: pool, workflow: store}, nil
}

func (c *SpecificationApprovalCoordinator) ApproveSpecification(
	ctx context.Context, request SpecificationApprovalRequest,
) (workflow.CommandResult, error) {
	if err := validateSpecificationApproval(request); err != nil {
		return workflow.CommandResult{}, err
	}
	return postgresutil.RetryTransaction(ctx, func() (workflow.CommandResult, error) {
		return c.approveOnce(ctx, request)
	})
}

func (c *SpecificationApprovalCoordinator) approveOnce(
	ctx context.Context, request SpecificationApprovalRequest,
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
	if err = c.fault(FaultSpecificationAfterWorkflow); err != nil {
		return workflow.CommandResult{}, err
	}
	if err = runtime.CheckpointSpecificationTx(ctx, tx, request.Command.ID,
		request.Command.Specification); err != nil {
		return workflow.CommandResult{}, err
	}
	if err = c.fault(FaultSpecificationAfterBinding); err != nil {
		return workflow.CommandResult{}, err
	}
	if err = runtime.EnqueueTx(ctx, tx, request.PlanningJob); err != nil {
		return workflow.CommandResult{}, err
	}
	if err = c.fault(FaultSpecificationAfterEnqueue); err != nil {
		return workflow.CommandResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return workflow.CommandResult{}, fmt.Errorf("commit specification approval: %w", err)
	}
	committed = true
	return result, nil
}

func (c *SpecificationApprovalCoordinator) fault(point SpecificationApprovalFaultPoint) error {
	if c.inject != nil {
		return c.inject(point)
	}
	return nil
}

func validateSpecificationApproval(request SpecificationApprovalRequest) error {
	job := request.PlanningJob
	if job.TaskID != nil || job.RunID != request.Command.ID || job.Attempt != 1 ||
		job.Stage != orchestration.StagePlanningReasoning ||
		job.AvailableAt.UTC() != request.Command.Meta.Timestamp.UTC() ||
		job.ID != orchestration.StableID(request.Command.ID, "-", "1",
			orchestration.StagePlanningReasoning, "job") {
		return workflow.ErrInvalid
	}
	return request.Command.Specification.Validate()
}
