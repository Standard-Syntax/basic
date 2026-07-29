//go:build integration

package verification

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func verificationIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	connectionString := os.Getenv("TEST_DATABASE_URL")
	if connectionString == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if err := Migrate(t.Context(), connectionString); err != nil {
		t.Fatalf("migrate verification ledger: %v", err)
	}
	pool, err := pgxpool.New(t.Context(), connectionString)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPostgresVerificationLedgerRecoveryReplayConflictAndImmutability(t *testing.T) {
	pool := verificationIntegrationPool(t)
	ledger := NewPostgresVerificationLedger(pool)
	start := VerificationStart{
		VerificationID: uuid.NewString(),
		RequestDigest:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Timestamp:      time.Now().UTC(), ReservationTTL: time.Minute,
	}
	handle, err := ledger.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	evidence := VerificationEvidence{
		ReportArtifact: workflow.ArtifactRef{
			URI:    "artifact://sha256/report",
			Digest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
		CandidateCommit: "cccccccccccccccccccccccccccccccccccccccc",
		Coverage: []CriterionCoverage{{
			CriterionID: "AC-001", CheckIDs: []string{"make-check-v1"},
			Covered: true, Passed: true,
		}},
		Passed: true,
	}
	if err := handle.SaveEvidence(t.Context(), evidence); err != nil {
		t.Fatal(err)
	}
	recovered, err := ledger.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := recovered.Evidence(); !ok ||
		!got.ReportArtifact.Equal(evidence.ReportArtifact) {
		t.Fatalf("evidence recovery = %#v %t", got, ok)
	}
	if _, err := recovered.FinalTransitionTime(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	result := Result{
		VerificationID:  start.VerificationID,
		CandidateCommit: evidence.CandidateCommit,
		ReportArtifact:  evidence.ReportArtifact, Coverage: evidence.Coverage, Passed: true,
	}
	if err := recovered.Complete(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	replayHandle, err := ledger.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	replayed, ok := replayHandle.Replay()
	if !ok || replayed.VerificationID != start.VerificationID {
		t.Fatalf("replay = %#v %t", replayed, ok)
	}
	conflict := start
	conflict.RequestDigest = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	if _, err := ledger.Begin(t.Context(), conflict); !errors.Is(err, ErrVerificationConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE verification_ledger
		SET evidence_json='{}'::jsonb WHERE verification_id=$1`, start.VerificationID); err == nil {
		t.Fatal("completed verification evidence was mutable")
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM verification_ledger
		WHERE verification_id=$1`, start.VerificationID); err == nil {
		t.Fatal("verification row was deletable")
	}
}

func TestPostgresVerificationLedgerRecoversOnlyExpiredPreEvidenceReservation(t *testing.T) {
	pool := verificationIntegrationPool(t)
	ledger := NewPostgresVerificationLedger(pool)
	start := VerificationStart{
		VerificationID: uuid.NewString(),
		RequestDigest:  "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		Timestamp:      time.Now().UTC(), ReservationTTL: time.Minute,
	}
	if _, err := ledger.Begin(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE verification_ledger
		SET reserved_until=clock_timestamp()-interval '1 second'
		WHERE verification_id=$1`, start.VerificationID); err != nil {
		t.Fatal(err)
	}
	recovered, err := ledger.Begin(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	if _, replay := recovered.Replay(); replay {
		t.Fatal("expired reservation replayed")
	}
}
