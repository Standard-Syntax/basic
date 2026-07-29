package approval

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/migration"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var approvalMigrationFiles embed.FS

func Migrate(ctx context.Context, connectionString string) error {
	return migration.Apply(ctx, connectionString, approvalMigrationFiles, "migrations")
}

type PostgresApprovalRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresApprovalRepository(
	pool *pgxpool.Pool,
) (*PostgresApprovalRepository, error) {
	if pool == nil {
		return nil, errors.New("PostgreSQL pool is required")
	}
	return &PostgresApprovalRepository{pool: pool}, nil
}

func (r *PostgresApprovalRepository) Begin(
	ctx context.Context, start ApprovalStart,
) (ApprovalHandle, error) {
	for {
		result, err := r.pool.Exec(ctx, `INSERT INTO task_approvals (
			approval_id,request_digest,requested_at,principal_id,run_id,task_id,
			candidate_commit,approved_specification_digest,approved_task_digest,
			implementation_digest,execution_digest,verification_digest,review_digest,state
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'reserved')
		ON CONFLICT (approval_id) DO NOTHING`,
			start.ApprovalID, start.RequestDigest, start.RequestedAt, start.PrincipalID,
			start.RunID, start.TaskID, start.CandidateCommit,
			start.ApprovedSpecificationDigest, start.ApprovedTaskDigest,
			start.ImplementationDigest, start.ExecutionDigest,
			start.VerificationDigest, start.ReviewDigest,
		)
		if err != nil {
			return nil, fmt.Errorf("reserve task approval: %w", err)
		}
		if result.RowsAffected() == 1 {
			return &postgresApprovalHandle{
				repository: r, start: start, owner: true,
			}, nil
		}
		handle, state, err := r.load(ctx, start)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return nil, err
		}
		if state != "reserved" {
			return handle, nil
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

func (r *PostgresApprovalRepository) load(
	ctx context.Context, start ApprovalStart,
) (*postgresApprovalHandle, string, error) {
	var (
		requestDigest, state, decision, reason, artifactURI, artifactDigest string
		elevated                                                            bool
		riskJSON                                                            []byte
		completedAt                                                         *time.Time
	)
	err := r.pool.QueryRow(ctx, `SELECT request_digest,state,
		COALESCE(decision,''),COALESCE(decision_reason,''),
		COALESCE(approval_artifact_uri,''),COALESCE(approval_artifact_digest,''),
		COALESCE(elevated,false),COALESCE(risk_reasons,'[]'::jsonb),completed_at
		FROM task_approvals WHERE approval_id=$1`, start.ApprovalID,
	).Scan(
		&requestDigest, &state, &decision, &reason, &artifactURI, &artifactDigest,
		&elevated, &riskJSON, &completedAt,
	)
	if err != nil {
		return nil, "", err
	}
	if requestDigest != start.RequestDigest {
		return nil, "", ErrApprovalConflict
	}
	handle := &postgresApprovalHandle{repository: r, start: start}
	if state == "decision_ready" || state == "completed" {
		var reasons []string
		if err := json.Unmarshal(riskJSON, &reasons); err != nil {
			return nil, "", fmt.Errorf("decode approval risk reasons: %w", err)
		}
		handle.decision = &DecisionCheckpoint{
			Result: Result{
				ApprovalID: start.ApprovalID, Decision: decision,
				ApprovalArtifact: workflow.ArtifactRef{URI: artifactURI, Digest: artifactDigest},
				Elevated:         elevated, RiskReasons: reasons,
			},
			Reason: reason,
		}
	}
	if state == "completed" {
		result := cloneResult(handle.decision.Result)
		handle.result = &result
	}
	return handle, state, nil
}

type postgresApprovalHandle struct {
	repository *PostgresApprovalRepository
	start      ApprovalStart
	owner      bool
	decision   *DecisionCheckpoint
	result     *Result
}

func (h *postgresApprovalHandle) Replay() (Result, bool) {
	if h.result == nil {
		return Result{}, false
	}
	return cloneResult(*h.result), true
}

func (h *postgresApprovalHandle) Decision() (DecisionCheckpoint, bool) {
	if h.decision == nil {
		return DecisionCheckpoint{}, false
	}
	return cloneCheckpoint(*h.decision), true
}

func (h *postgresApprovalHandle) SaveDecision(
	ctx context.Context, checkpoint DecisionCheckpoint,
) error {
	if !h.owner || h.decision != nil {
		return ErrApprovalState
	}
	riskJSON, err := json.Marshal(checkpoint.Result.RiskReasons)
	if err != nil {
		return err
	}
	result, err := h.repository.pool.Exec(ctx, `UPDATE task_approvals SET
		state='decision_ready',decision=$2,decision_reason=$3,
		approval_artifact_uri=$4,approval_artifact_digest=$5,elevated=$6,risk_reasons=$7
		WHERE approval_id=$1 AND state='reserved'`,
		h.start.ApprovalID, checkpoint.Result.Decision, checkpoint.Reason,
		checkpoint.Result.ApprovalArtifact.URI, checkpoint.Result.ApprovalArtifact.Digest,
		checkpoint.Result.Elevated, riskJSON,
	)
	if err != nil {
		return fmt.Errorf("checkpoint task approval: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrApprovalState
	}
	value := cloneCheckpoint(checkpoint)
	h.decision = &value
	h.owner = false
	return nil
}

func (h *postgresApprovalHandle) Complete(ctx context.Context, result Result) error {
	if h.decision == nil {
		return ErrApprovalState
	}
	command, err := h.repository.pool.Exec(ctx, `UPDATE task_approvals SET
		state='completed',completed_at=clock_timestamp()
		WHERE approval_id=$1 AND state='decision_ready'`, h.start.ApprovalID)
	if err != nil {
		return fmt.Errorf("complete task approval: %w", err)
	}
	if command.RowsAffected() != 1 {
		loaded, state, loadErr := h.repository.load(ctx, h.start)
		if loadErr == nil && state == "completed" &&
			loaded.result.ApprovalArtifact.Equal(result.ApprovalArtifact) &&
			loaded.result.Decision == result.Decision {
			return nil
		}
		return ErrApprovalConflict
	}
	value := cloneResult(result)
	h.result = &value
	return nil
}

func (h *postgresApprovalHandle) Rollback(ctx context.Context) error {
	if !h.owner || h.decision != nil || h.result != nil {
		return nil
	}
	result, err := h.repository.pool.Exec(ctx,
		`DELETE FROM task_approvals WHERE approval_id=$1 AND state='reserved'`,
		h.start.ApprovalID,
	)
	if err != nil {
		return fmt.Errorf("rollback task approval reservation: %w", err)
	}
	if result.RowsAffected() == 1 {
		h.owner = false
	}
	return nil
}
