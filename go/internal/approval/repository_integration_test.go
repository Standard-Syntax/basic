//go:build integration

package approval

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func approvalRepositoryFixture(
	t *testing.T,
) (*PostgresApprovalRepository, *pgxpool.Pool) {
	t.Helper()
	connectionString := os.Getenv("TEST_DATABASE_URL")
	if connectionString == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if err := Migrate(t.Context(), connectionString); err != nil {
		t.Fatalf("migrate approvals: %v", err)
	}
	pool, err := pgxpool.New(t.Context(), connectionString)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := NewPostgresApprovalRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return repository, pool
}

func approvalStart() ApprovalStart {
	return ApprovalStart{
		ApprovalID: uuid.NewString(), RequestDigest: repeat('a', 64),
		RequestedAt: time.Now().UTC(), PrincipalID: uuid.NewString(),
		RunID: "run-1", TaskID: "TASK-001",
		CandidateCommit:             repeat('b', 40),
		ApprovedSpecificationDigest: repeat('c', 64),
		ApprovedTaskDigest:          repeat('d', 64),
		ImplementationDigest:        repeat('e', 64), ExecutionDigest: repeat('f', 64),
		VerificationDigest: repeat('1', 64), ReviewDigest: repeat('2', 64),
	}
}

func TestPostgresApprovalCheckpointReplayConflictAndImmutability(t *testing.T) {
	repository, pool := approvalRepositoryFixture(t)
	start := approvalStart()
	handle, err := repository.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	artifact := workflow.ArtifactRef{
		URI: "artifact://sha256/" + repeat('3', 64), Digest: repeat('3', 64),
	}
	checkpoint := DecisionCheckpoint{Result: Result{
		ApprovalID: start.ApprovalID, Decision: "approve",
		ApprovalArtifact: artifact, Elevated: true,
		RiskReasons: []string{"path:go.mod"},
	}}
	if err := handle.SaveDecision(t.Context(), checkpoint); err != nil {
		t.Fatal(err)
	}
	recovery, err := repository.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := recovery.Decision(); !ok || !value.Result.ApprovalArtifact.Equal(artifact) {
		t.Fatalf("decision checkpoint=%#v ready=%v", value, ok)
	}
	if err := recovery.Complete(t.Context(), checkpoint.Result); err != nil {
		t.Fatal(err)
	}
	replay, err := repository.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	if result, ok := replay.Replay(); !ok || !result.ApprovalArtifact.Equal(artifact) {
		t.Fatalf("completed replay=%#v ok=%v", result, ok)
	}
	conflict := start
	conflict.RequestDigest = repeat('9', 64)
	if _, err := repository.Begin(t.Context(), conflict); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("conflicting reuse err=%v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE task_approvals
		SET candidate_commit=$2 WHERE approval_id=$1`, start.ApprovalID, repeat('4', 40)); err == nil {
		t.Fatal("immutable approval identity was updated")
	}
	if _, err := pool.Exec(t.Context(),
		`DELETE FROM task_approvals WHERE approval_id=$1`, start.ApprovalID,
	); err == nil {
		t.Fatal("completed approval was deleted")
	}
}

func TestPostgresApprovalReservationRollback(t *testing.T) {
	repository, pool := approvalRepositoryFixture(t)
	start := approvalStart()
	handle, err := repository.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM task_approvals WHERE approval_id=$1`, start.ApprovalID,
	).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled back reservation count=%d err=%v", count, err)
	}
}

func TestPostgresConcurrentIdenticalApprovalIsOneLogicalDecision(t *testing.T) {
	repository, _ := approvalRepositoryFixture(t)
	_, request, store, workflowPort := approvalFixture(t)
	service, err := NewService(store, workflowPort, repository)
	if err != nil {
		t.Fatal(err)
	}
	const callers = 8
	results := make(chan Result, callers)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			result, err := service.ApproveTask(context.Background(), request)
			results <- result
			errs <- err
		}()
	}
	var artifact workflow.ArtifactRef
	for range callers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		result := <-results
		if artifact.URI == "" {
			artifact = result.ApprovalArtifact
		}
		if !artifact.Equal(result.ApprovalArtifact) {
			t.Fatalf("approval artifact mismatch: %v %v", artifact, result.ApprovalArtifact)
		}
	}
	if workflowPort.calls != 1 {
		t.Fatalf("logical decisions=%d", workflowPort.calls)
	}
}
