//go:build integration

package release

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/artifact"
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func releaseIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	connectionString := os.Getenv("TEST_DATABASE_URL")
	if connectionString == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if err := verification.Migrate(t.Context(), connectionString); err != nil {
		t.Fatalf("migrate verification ledger: %v", err)
	}
	pool, err := pgxpool.New(t.Context(), connectionString)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestVerificationReadinessRejectsMultipleCompletedAttemptsForCandidate(t *testing.T) {
	pool := releaseIntegrationPool(t)
	store, err := artifact.NewStore(t.TempDir(), artifact.DefaultMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	manifest := testManifest()
	passingID := uuid.NewString()
	report := verification.VerificationReport{
		SchemaVersion: "1", VerificationID: passingID, RunID: manifest.Canary.RunID,
		TaskID: manifest.Canary.TaskID, CandidateCommit: manifest.Canary.CandidateCommit, Passed: true,
	}
	reportBody, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Canary.Verification, err = store.Put(t.Context(), reportBody)
	if err != nil {
		t.Fatal(err)
	}
	passing := verification.Result{
		VerificationID: passingID, CandidateCommit: manifest.Canary.CandidateCommit,
		ReportArtifact: manifest.Canary.Verification, Passed: true,
	}
	insertCompletedVerification(t, pool, passing)
	if err := verifyVerificationEvidence(t.Context(), pool, store, &manifest); err != nil {
		t.Fatalf("single exact completed verification failed: %v", err)
	}

	failedAttempt := verification.Result{
		VerificationID: uuid.NewString(), CandidateCommit: manifest.Canary.CandidateCommit,
		ReportArtifact: manifest.Canary.Verification, Passed: false,
	}
	insertCompletedVerification(t, pool, failedAttempt)
	if err := verifyVerificationEvidence(t.Context(), pool, store, &manifest); err == nil ||
		err.Error() != "verification ledger binding mismatch" {
		t.Fatalf("duplicate candidate verification error = %v", err)
	}
}

func insertCompletedVerification(t *testing.T, pool *pgxpool.Pool, result verification.Result) {
	t.Helper()
	resultBody, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err = pool.Exec(t.Context(), `INSERT INTO verification_ledger (
		verification_id,request_digest,owner_id,reserved_until,state,verification_timestamp,
		evidence_json,final_transition_at,result_json,evidence_ready_at,completed_at
	) VALUES ($1,$2,$3,$4,'completed',$4,'{}'::jsonb,$4,$5,$4,$4)`,
		result.VerificationID, strings.Repeat("a", 64), uuid.NewString(), now, resultBody)
	if err != nil {
		t.Fatal(err)
	}
}
