//go:build integration

package publication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func publicationRepositoryFixture(
	t *testing.T,
) (*PostgresPublicationRepository, *pgxpool.Pool, string) {
	t.Helper()
	connectionString := os.Getenv("TEST_DATABASE_URL")
	if connectionString == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if err := Migrate(t.Context(), connectionString); err != nil {
		t.Fatalf("migrate publications: %v", err)
	}
	pool, err := pgxpool.New(t.Context(), connectionString)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := NewPostgresPublicationRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return repository, pool, connectionString
}

func publicationStart() PublicationStart {
	return PublicationStart{
		PublicationID: uuid.NewString(), RequestDigest: repeated('a', 64),
		RequestedAt: time.Now().UTC(), Repository: "owner/repo",
		BaseBranch: "main", HeadBranch: "harness/" + uuid.NewString(),
		BaseCommit: repeated('b', 40), CandidateCommit: repeated('c', 40),
		SpecificationDigest: repeated('d', 64), ImplementationDigest: repeated('e', 64),
		ExecutionDigest: repeated('f', 64), VerificationDigest: repeated('1', 64),
		ReviewDigest: repeated('2', 64), ApprovalDigest: repeated('3', 64),
		ExpectedRunRevision: 12,
	}
}

func TestPostgresPublicationCheckpointsReplayConflictAndImmutability(t *testing.T) {
	repository, pool, _ := publicationRepositoryFixture(t)
	start := publicationStart()
	handle, err := repository.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	branch := BranchCheckpoint{Branch: start.HeadBranch, CandidateCommit: start.CandidateCommit}
	if err := handle.SaveBranch(t.Context(), branch); err != nil {
		t.Fatal(err)
	}
	recovery, err := repository.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := recovery.Branch(); !ok || got != branch {
		t.Fatalf("branch checkpoint=%#v ok=%v", got, ok)
	}
	pull := PullRequestCheckpoint{
		Branch: branch.Branch, CandidateCommit: branch.CandidateCommit,
		PullRequestNumber: 52, PullRequestURL: "https://example.invalid/pull/52",
	}
	if err := recovery.SavePullRequest(t.Context(), pull); err != nil {
		t.Fatal(err)
	}
	prRecovery, err := repository.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := prRecovery.PullRequest(); !ok || got != pull {
		t.Fatalf("PR checkpoint=%#v ok=%v", got, ok)
	}
	artifact := workflow.ArtifactRef{
		URI: "artifact://sha256/" + repeated('4', 64), Digest: repeated('4', 64),
	}
	result := Result{
		PublicationID: start.PublicationID, Branch: branch.Branch,
		CandidateCommit: branch.CandidateCommit, PullRequestNumber: pull.PullRequestNumber,
		PullRequestURL: pull.PullRequestURL, PublicationArtifact: artifact,
	}
	if err := prRecovery.Complete(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	replay, err := repository.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := replay.Replay(); !ok || !got.PublicationArtifact.Equal(artifact) {
		t.Fatalf("replay=%#v ok=%v", got, ok)
	}
	completed, err := repository.LoadCompleted(t.Context(), start.PublicationID)
	if err != nil || completed.Repository != start.Repository ||
		completed.HeadBranch != start.HeadBranch || completed.CandidateCommit != start.CandidateCommit ||
		completed.PullRequestNumber != pull.PullRequestNumber ||
		!completed.PublicationArtifact.Equal(artifact) {
		t.Fatalf("completed publication=%#v err=%v", completed, err)
	}
	conflict := start
	conflict.RequestDigest = repeated('9', 64)
	if _, err := repository.Begin(t.Context(), conflict); !errors.Is(err, ErrPublicationConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE draft_pull_request_publications
		SET candidate_commit=$2 WHERE publication_id=$1`,
		start.PublicationID, repeated('5', 40)); err == nil {
		t.Fatal("immutable identity was updated")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE draft_pull_request_publications
		SET pull_request_number=53 WHERE publication_id=$1`, start.PublicationID); err == nil {
		t.Fatal("checkpointed PR identity was updated")
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM draft_pull_request_publications
		WHERE publication_id=$1`, start.PublicationID); err == nil {
		t.Fatal("completed publication was deleted")
	}
}

func TestPostgresPublicationRollbackAndConcurrentCheckpoint(t *testing.T) {
	repository, pool, _ := publicationRepositoryFixture(t)
	rollback := publicationStart()
	handle, err := repository.Begin(t.Context(), rollback)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM draft_pull_request_publications
		WHERE publication_id=$1`, rollback.PublicationID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback count=%d err=%v", count, err)
	}

	start := publicationStart()
	const callers = 8
	var wait sync.WaitGroup
	errs := make(chan error, callers)
	checkpoint := BranchCheckpoint{Branch: start.HeadBranch, CandidateCommit: start.CandidateCommit}
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			value, beginErr := repository.Begin(context.Background(), start)
			if beginErr != nil {
				errs <- beginErr
				return
			}
			errs <- value.SaveBranch(context.Background(), checkpoint)
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM draft_pull_request_publications
		WHERE publication_id=$1 AND state='branch_ready'`,
		start.PublicationID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("checkpoint rows=%d err=%v", count, err)
	}
}

func TestPublicationMigrationReplayAndDigestProtection(t *testing.T) {
	_, pool, connectionString := publicationRepositoryFixture(t)
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- Migrate(context.Background(), connectionString) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var original string
	if err := pool.QueryRow(t.Context(),
		`SELECT digest FROM schema_migrations WHERE version=13`,
	).Scan(&original); err != nil || len(original) != 64 {
		t.Fatalf("migration digest=%q err=%v", original, err)
	}
	if _, err := pool.Exec(t.Context(),
		`UPDATE schema_migrations SET digest=$1 WHERE version=13`, repeated('0', 64)); err != nil {
		t.Fatal(err)
	}
	migrationErr := Migrate(t.Context(), connectionString)
	if _, err := pool.Exec(t.Context(),
		`UPDATE schema_migrations SET digest=$1 WHERE version=13`, original); err != nil {
		t.Fatal(err)
	}
	if migrationErr == nil {
		t.Fatal("modified migration digest was accepted")
	}
	body, err := publicationMigrationFiles.ReadFile("migrations/0013_publications.sql")
	if err != nil {
		t.Fatal(err)
	}
	if original != fmt.Sprintf("%x", sha256.Sum256(body)) {
		t.Fatal("migration ledger digest differs from embedded SQL")
	}
}

func repeated(value byte, count int) string {
	return string(bytes.Repeat([]byte{value}, count))
}
