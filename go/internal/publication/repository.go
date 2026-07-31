package publication

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/migration"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var publicationMigrationFiles embed.FS

func MigrationSource() migration.Source {
	return migration.Source{Files: publicationMigrationFiles, Directory: "migrations"}
}

func Migrate(ctx context.Context, connectionString string) error {
	return migration.Apply(ctx, connectionString, publicationMigrationFiles, "migrations")
}

type PostgresPublicationRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresPublicationRepository(
	pool *pgxpool.Pool,
) (*PostgresPublicationRepository, error) {
	if pool == nil {
		return nil, errors.New("PostgreSQL pool is required")
	}
	return &PostgresPublicationRepository{pool: pool}, nil
}

func (r *PostgresPublicationRepository) Begin(
	ctx context.Context, start PublicationStart,
) (PublicationHandle, error) {
	result, err := r.pool.Exec(ctx, `INSERT INTO draft_pull_request_publications (
		publication_id,request_digest,requested_at,repository,base_branch,head_branch,
		base_commit,candidate_commit,specification_digest,implementation_digest,
		execution_digest,verification_digest,review_digest,approval_digest,
		expected_run_revision,state
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'reserved')
	ON CONFLICT (publication_id) DO NOTHING`,
		start.PublicationID, start.RequestDigest, start.RequestedAt, start.Repository,
		start.BaseBranch, start.HeadBranch, start.BaseCommit, start.CandidateCommit,
		start.SpecificationDigest, start.ImplementationDigest, start.ExecutionDigest,
		start.VerificationDigest, start.ReviewDigest, start.ApprovalDigest,
		start.ExpectedRunRevision,
	)
	if err != nil {
		return nil, fmt.Errorf("reserve publication: %w", err)
	}
	handle, state, err := r.load(ctx, start)
	if err != nil {
		return nil, err
	}
	if result.RowsAffected() == 1 || state == "reserved" {
		handle.owner = true
	}
	return handle, nil
}

func (r *PostgresPublicationRepository) load(
	ctx context.Context, start PublicationStart,
) (*postgresPublicationHandle, string, error) {
	var (
		digest, state, branch, candidate, pullURL, artifactURI, artifactDigest string
		pullNumber                                                             int64
	)
	err := r.pool.QueryRow(ctx, `SELECT request_digest,state,
		COALESCE(published_branch,''),COALESCE(published_candidate_commit,''),
		COALESCE(pull_request_number,0),COALESCE(pull_request_url,''),
		COALESCE(publication_artifact_uri,''),COALESCE(publication_artifact_digest,'')
		FROM draft_pull_request_publications WHERE publication_id=$1`,
		start.PublicationID,
	).Scan(
		&digest, &state, &branch, &candidate, &pullNumber, &pullURL,
		&artifactURI, &artifactDigest,
	)
	if err != nil {
		return nil, "", err
	}
	if digest != start.RequestDigest {
		return nil, "", ErrPublicationConflict
	}
	handle := &postgresPublicationHandle{repository: r, start: start}
	if state == "branch_ready" || state == "pr_ready" || state == "completed" {
		handle.branch = &BranchCheckpoint{Branch: branch, CandidateCommit: candidate}
	}
	if state == "pr_ready" || state == "completed" {
		handle.pull = &PullRequestCheckpoint{
			Branch: branch, CandidateCommit: candidate,
			PullRequestNumber: pullNumber, PullRequestURL: pullURL,
		}
	}
	if state == "completed" {
		handle.result = &Result{
			PublicationID: start.PublicationID, Branch: branch,
			CandidateCommit: candidate, PullRequestNumber: pullNumber,
			PullRequestURL:      pullURL,
			PublicationArtifact: workflow.ArtifactRef{URI: artifactURI, Digest: artifactDigest},
		}
	}
	return handle, state, nil
}

type postgresPublicationHandle struct {
	repository *PostgresPublicationRepository
	start      PublicationStart
	owner      bool
	branch     *BranchCheckpoint
	pull       *PullRequestCheckpoint
	result     *Result
}

func (h *postgresPublicationHandle) Replay() (Result, bool) {
	if h.result == nil {
		return Result{}, false
	}
	return *h.result, true
}

func (h *postgresPublicationHandle) Branch() (BranchCheckpoint, bool) {
	if h.branch == nil {
		return BranchCheckpoint{}, false
	}
	return *h.branch, true
}

func (h *postgresPublicationHandle) SaveBranch(
	ctx context.Context, checkpoint BranchCheckpoint,
) error {
	result, err := h.repository.pool.Exec(ctx, `UPDATE draft_pull_request_publications
		SET state='branch_ready',published_branch=$2,published_candidate_commit=$3
		WHERE publication_id=$1 AND state='reserved'`,
		h.start.PublicationID, checkpoint.Branch, checkpoint.CandidateCommit)
	if err != nil {
		return fmt.Errorf("checkpoint publication branch: %w", err)
	}
	if result.RowsAffected() != 1 {
		loaded, state, loadErr := h.repository.load(ctx, h.start)
		if loadErr != nil || state == "reserved" || loaded.branch == nil ||
			*loaded.branch != checkpoint {
			return ErrPublicationState
		}
	}
	value := checkpoint
	h.branch = &value
	h.owner = false
	return nil
}

func (h *postgresPublicationHandle) PullRequest() (PullRequestCheckpoint, bool) {
	if h.pull == nil {
		return PullRequestCheckpoint{}, false
	}
	return *h.pull, true
}

func (h *postgresPublicationHandle) SavePullRequest(
	ctx context.Context, checkpoint PullRequestCheckpoint,
) error {
	result, err := h.repository.pool.Exec(ctx, `UPDATE draft_pull_request_publications
		SET state='pr_ready',pull_request_number=$2,pull_request_url=$3
		WHERE publication_id=$1 AND state='branch_ready'
		AND published_branch=$4 AND published_candidate_commit=$5`,
		h.start.PublicationID, checkpoint.PullRequestNumber, checkpoint.PullRequestURL,
		checkpoint.Branch, checkpoint.CandidateCommit)
	if err != nil {
		return fmt.Errorf("checkpoint pull request: %w", err)
	}
	if result.RowsAffected() != 1 {
		loaded, state, loadErr := h.repository.load(ctx, h.start)
		if loadErr != nil || state == "branch_ready" || loaded.pull == nil ||
			*loaded.pull != checkpoint {
			return ErrPublicationState
		}
	}
	value := checkpoint
	h.pull = &value
	return nil
}

func (h *postgresPublicationHandle) Complete(ctx context.Context, result Result) error {
	command, err := h.repository.pool.Exec(ctx, `UPDATE draft_pull_request_publications
		SET state='completed',publication_artifact_uri=$2,
		publication_artifact_digest=$3,completed_at=$4
		WHERE publication_id=$1 AND state='pr_ready'`,
		h.start.PublicationID, result.PublicationArtifact.URI,
		result.PublicationArtifact.Digest, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("complete publication: %w", err)
	}
	if command.RowsAffected() != 1 {
		loaded, state, loadErr := h.repository.load(ctx, h.start)
		if loadErr != nil || state != "completed" || loaded.result == nil ||
			!loaded.result.PublicationArtifact.Equal(result.PublicationArtifact) {
			return ErrPublicationConflict
		}
		return nil
	}
	value := result
	h.result = &value
	return nil
}

func (h *postgresPublicationHandle) Rollback(ctx context.Context) error {
	if !h.owner || h.branch != nil || h.pull != nil || h.result != nil {
		return nil
	}
	_, err := h.repository.pool.Exec(ctx,
		`DELETE FROM draft_pull_request_publications
		WHERE publication_id=$1 AND state='reserved'`, h.start.PublicationID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("rollback publication reservation: %w", err)
	}
	h.owner = false
	return nil
}
