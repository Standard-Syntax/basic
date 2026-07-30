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
	RunID                 string
	Intake                workflow.ArtifactRef
	BaseCommit            string
	RepositoryMap         *workflow.ArtifactRef
	ApprovedSpecification *workflow.ArtifactRef
	ApprovedTaskGraph     *workflow.ArtifactRef
	CompositeApproval     *workflow.ArtifactRef
	CreatedAt             time.Time
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
	if err := binding.Intake.Validate(); err != nil {
		return err
	}
	tag, err := r.pool.Exec(ctx, `INSERT INTO runtime_run_bindings
		(run_id,intake_uri,intake_digest,base_commit,created_at)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
		binding.RunID, binding.Intake.URI, binding.Intake.Digest,
		binding.BaseCommit, binding.CreatedAt.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	existing, err := r.GetRun(ctx, binding.RunID)
	if err != nil {
		return err
	}
	if existing.Intake != binding.Intake || existing.BaseCommit != binding.BaseCommit ||
		!existing.CreatedAt.Equal(binding.CreatedAt.UTC()) {
		return ErrConflict
	}
	return nil
}

func (r *BindingRepository) GetRun(ctx context.Context, runID string) (RunBinding, error) {
	var result RunBinding
	var repositoryURI, repositoryDigest, specificationURI, specificationDigest *string
	var graphURI, graphDigest, approvalURI, approvalDigest *string
	err := r.pool.QueryRow(ctx, `SELECT run_id::text,intake_uri,intake_digest,base_commit,
		repository_map_uri,repository_map_digest,approved_specification_uri,
		approved_specification_digest,approved_task_graph_uri,approved_task_graph_digest,
		composite_approval_uri,composite_approval_digest,created_at
		FROM runtime_run_bindings WHERE run_id=$1`, runID).Scan(
		&result.RunID, &result.Intake.URI, &result.Intake.Digest, &result.BaseCommit,
		&repositoryURI, &repositoryDigest, &specificationURI, &specificationDigest,
		&graphURI, &graphDigest, &approvalURI, &approvalDigest, &result.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunBinding{}, ErrNotFound
	}
	if err != nil {
		return RunBinding{}, err
	}
	result.RepositoryMap = optionalRef(repositoryURI, repositoryDigest)
	result.ApprovedSpecification = optionalRef(specificationURI, specificationDigest)
	result.ApprovedTaskGraph = optionalRef(graphURI, graphDigest)
	result.CompositeApproval = optionalRef(approvalURI, approvalDigest)
	return result, nil
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
	if err := graph.Validate(); err != nil {
		return err
	}
	if err := task.ApprovedTask.Validate(); err != nil || task.RunID != runID {
		if err != nil {
			return err
		}
		return ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
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
	return tx.Commit(ctx)
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
