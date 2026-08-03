package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RunBinding struct {
	RunID                   string
	Intake                  workflow.ArtifactRef
	BaseCommit              string
	RepositoryMap           *workflow.ArtifactRef
	Policy                  *workflow.ArtifactRef
	ExecutionImageDigest    string
	VerificationImageDigest string
	ApprovedSpecification   *workflow.ArtifactRef
	ApprovedTaskGraph       *workflow.ArtifactRef
	CompositeApproval       *workflow.ArtifactRef
	CreatedAt               time.Time
}

type TaskBinding struct {
	RunID, TaskID string
	ApprovedTask  workflow.ArtifactRef
}

type BindingRepository struct{ pool *pgxpool.Pool }

func NewBindingRepository(pool *pgxpool.Pool) *BindingRepository {
	return &BindingRepository{pool: pool}
}

func (r *BindingRepository) CreateRun(ctx context.Context, binding RunBinding) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := CreateRunBindingTx(ctx, tx, binding); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateRunBindingTx inserts the complete immutable intake binding in a
// caller-owned transaction. RepositoryMap is mandatory for newly accepted
// runs; nullable database columns remain only for legacy migration tolerance.
func CreateRunBindingTx(ctx context.Context, tx pgx.Tx, binding RunBinding) error {
	if err := validateIntakeBinding(binding); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO runtime_run_bindings
		(run_id,intake_uri,intake_digest,base_commit,repository_map_uri,
		 repository_map_digest,beta_policy_uri,beta_policy_digest,
		 execution_image_digest,verification_image_digest,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`,
		binding.RunID, binding.Intake.URI, binding.Intake.Digest, binding.BaseCommit,
		binding.RepositoryMap.URI, binding.RepositoryMap.Digest,
		binding.Policy.URI, binding.Policy.Digest, binding.ExecutionImageDigest,
		binding.VerificationImageDigest, binding.CreatedAt.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	existing, err := readIntakeBinding(ctx, tx, binding.RunID)
	if err != nil {
		return err
	}
	if !sameIntakeBinding(existing, binding) {
		return ErrConflict
	}
	return nil
}

func validateIntakeBinding(binding RunBinding) error {
	if err := binding.Intake.Validate(); err != nil {
		return err
	}
	if binding.RepositoryMap == nil || binding.Policy == nil ||
		!validImageDigest(binding.ExecutionImageDigest) || !validImageDigest(binding.VerificationImageDigest) {
		return ErrConflict
	}
	if err := binding.RepositoryMap.Validate(); err != nil {
		return err
	}
	return binding.Policy.Validate()
}

func readIntakeBinding(ctx context.Context, tx pgx.Tx, runID string) (RunBinding, error) {
	var existing RunBinding
	var repositoryURI, repositoryDigest, policyURI, policyDigest *string
	var executionImageDigest, verificationImageDigest *string
	err := tx.QueryRow(ctx, `SELECT run_id::text,intake_uri,intake_digest,base_commit,
		repository_map_uri,repository_map_digest,beta_policy_uri,beta_policy_digest,
		execution_image_digest,verification_image_digest,created_at
		FROM runtime_run_bindings WHERE run_id=$1`, runID).Scan(
		&existing.RunID, &existing.Intake.URI, &existing.Intake.Digest,
		&existing.BaseCommit, &repositoryURI, &repositoryDigest, &policyURI, &policyDigest,
		&executionImageDigest, &verificationImageDigest, &existing.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunBinding{}, ErrNotFound
	}
	if err != nil {
		return RunBinding{}, err
	}
	existing.RepositoryMap = optionalRef(repositoryURI, repositoryDigest)
	existing.Policy = optionalRef(policyURI, policyDigest)
	existing.ExecutionImageDigest = optionalString(executionImageDigest)
	existing.VerificationImageDigest = optionalString(verificationImageDigest)
	return existing, nil
}

func sameIntakeBinding(existing, binding RunBinding) bool {
	return existing.Intake == binding.Intake && existing.BaseCommit == binding.BaseCommit &&
		existing.RepositoryMap != nil && *existing.RepositoryMap == *binding.RepositoryMap &&
		existing.Policy != nil && *existing.Policy == *binding.Policy &&
		existing.ExecutionImageDigest == binding.ExecutionImageDigest &&
		existing.VerificationImageDigest == binding.VerificationImageDigest &&
		existing.CreatedAt.Equal(binding.CreatedAt.UTC())
}

func (r *BindingRepository) GetRun(ctx context.Context, runID string) (RunBinding, error) {
	var result RunBinding
	var repositoryURI, repositoryDigest, policyURI, policyDigest *string
	var specificationURI, specificationDigest *string
	var graphURI, graphDigest, approvalURI, approvalDigest *string
	var executionImageDigest, verificationImageDigest *string
	err := r.pool.QueryRow(ctx, `SELECT run_id::text,intake_uri,intake_digest,base_commit,
		repository_map_uri,repository_map_digest,beta_policy_uri,beta_policy_digest,
		execution_image_digest,verification_image_digest,approved_specification_uri,
		approved_specification_digest,approved_task_graph_uri,approved_task_graph_digest,
		composite_approval_uri,composite_approval_digest,created_at
		FROM runtime_run_bindings WHERE run_id=$1`, runID).Scan(
		&result.RunID, &result.Intake.URI, &result.Intake.Digest, &result.BaseCommit,
		&repositoryURI, &repositoryDigest, &policyURI, &policyDigest,
		&executionImageDigest, &verificationImageDigest,
		&specificationURI, &specificationDigest,
		&graphURI, &graphDigest, &approvalURI, &approvalDigest, &result.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunBinding{}, ErrNotFound
	}
	if err != nil {
		return RunBinding{}, err
	}
	result.RepositoryMap = optionalRef(repositoryURI, repositoryDigest)
	result.Policy = optionalRef(policyURI, policyDigest)
	result.ExecutionImageDigest = optionalString(executionImageDigest)
	result.VerificationImageDigest = optionalString(verificationImageDigest)
	result.ApprovedSpecification = optionalRef(specificationURI, specificationDigest)
	result.ApprovedTaskGraph = optionalRef(graphURI, graphDigest)
	result.CompositeApproval = optionalRef(approvalURI, approvalDigest)
	return result, nil
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (r *BindingRepository) CheckpointRepository(
	ctx context.Context, runID string, ref workflow.ArtifactRef,
) error {
	return r.checkpointRun(ctx, runID, "repository_map_uri", "repository_map_digest", ref)
}

func (r *BindingRepository) CheckpointSpecification(
	ctx context.Context, runID string, ref workflow.ArtifactRef,
) error {
	return r.checkpointRun(
		ctx, runID, "approved_specification_uri", "approved_specification_digest", ref,
	)
}

func (r *BindingRepository) CheckpointTaskGraph(
	ctx context.Context, runID string, graph workflow.ArtifactRef, task TaskBinding,
) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := CheckpointTaskGraphTx(ctx, tx, runID, graph, task); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CheckpointTaskGraphTx records the immutable graph and task bindings inside a
// caller-owned transaction.
func CheckpointTaskGraphTx(
	ctx context.Context, tx pgx.Tx, runID string, graph workflow.ArtifactRef, task TaskBinding,
) error {
	if tx == nil {
		return ErrConflict
	}
	if err := graph.Validate(); err != nil {
		return err
	}
	if err := task.ApprovedTask.Validate(); err != nil || task.RunID != runID {
		if err != nil {
			return err
		}
		return ErrConflict
	}
	if err := checkpointRunTx(
		ctx, tx, runID, "approved_task_graph_uri", "approved_task_graph_digest", graph,
	); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO runtime_task_bindings
		(task_id,run_id,approved_task_uri,approved_task_digest)
		VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		task.TaskID, runID, task.ApprovedTask.URI, task.ApprovedTask.Digest)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var existing TaskBinding
		err = tx.QueryRow(ctx, `SELECT run_id::text,task_id::text,
			approved_task_uri,approved_task_digest
			FROM runtime_task_bindings WHERE task_id=$1`, task.TaskID).Scan(
			&existing.RunID, &existing.TaskID,
			&existing.ApprovedTask.URI, &existing.ApprovedTask.Digest)
		if err != nil {
			return err
		}
		if existing != task {
			return ErrConflict
		}
	}
	return nil
}

func (r *BindingRepository) CheckpointApproval(
	ctx context.Context, runID string, ref workflow.ArtifactRef,
) error {
	return r.checkpointRun(
		ctx, runID, "composite_approval_uri", "composite_approval_digest", ref,
	)
}

func (r *BindingRepository) GetTask(
	ctx context.Context, runID, taskID string,
) (TaskBinding, error) {
	var result TaskBinding
	err := r.pool.QueryRow(ctx, `SELECT run_id::text,task_id::text,
		approved_task_uri,approved_task_digest FROM runtime_task_bindings
		WHERE run_id=$1 AND task_id=$2`, runID, taskID).Scan(
		&result.RunID, &result.TaskID, &result.ApprovedTask.URI, &result.ApprovedTask.Digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskBinding{}, ErrNotFound
	}
	return result, err
}

func (r *BindingRepository) checkpointRun(
	ctx context.Context, runID, uriColumn, digestColumn string, ref workflow.ArtifactRef,
) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := checkpointRunTx(ctx, tx, runID, uriColumn, digestColumn, ref); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func checkpointRunTx(
	ctx context.Context, tx pgx.Tx, runID, uriColumn, digestColumn string,
	ref workflow.ArtifactRef,
) error {
	allowed := map[string]bool{
		"repository_map_uri:repository_map_digest":                 true,
		"approved_specification_uri:approved_specification_digest": true,
		"approved_task_graph_uri:approved_task_graph_digest":       true,
		"composite_approval_uri:composite_approval_digest":         true,
	}
	if !allowed[uriColumn+":"+digestColumn] {
		return ErrConflict
	}
	query := `UPDATE runtime_run_bindings SET ` + uriColumn + `=$2,` + digestColumn +
		`=$3 WHERE run_id=$1 AND ` + uriColumn + ` IS NULL`
	tag, err := tx.Exec(ctx, query, runID, ref.URI, ref.Digest)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var uri, digest string
	err = tx.QueryRow(ctx, `SELECT `+uriColumn+`,`+digestColumn+
		` FROM runtime_run_bindings WHERE run_id=$1`, runID).Scan(&uri, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if uri != ref.URI || digest != ref.Digest {
		return ErrConflict
	}
	return nil
}

func optionalRef(uri, digest *string) *workflow.ArtifactRef {
	if uri == nil || digest == nil {
		return nil
	}
	return &workflow.ArtifactRef{URI: *uri, Digest: *digest}
}

func validImageDigest(value string) bool {
	if len(value) != 71 || value[:7] != "sha256:" {
		return false
	}
	for _, item := range value[7:] {
		if !((item >= '0' && item <= '9') || (item >= 'a' && item <= 'f')) {
			return false
		}
	}
	return true
}
