//go:build integration

package gateway

import (
	"errors"
	"os"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func integrationService(
	t *testing.T,
) (*Service, *pgxpool.Pool, *countingAdapter) {
	t.Helper()
	connectionString := os.Getenv("TEST_DATABASE_URL")
	if connectionString == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if err := Migrate(t.Context(), connectionString); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(t.Context(), connectionString)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := NewPostgresInvocationRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	request := gatewayRequest(t)
	resolver := &fakeResolver{resolved: ResolvedManifest{
		Digest:   request.GetEnvelope().GetAgentManifestDigest(),
		Manifest: implementationManifest(t),
	}}
	fake, err := NewFakeImplementationAdapter(
		gatewayProposal(t), "fake-implementation-v1",
		Usage{InputTokens: 11, OutputTokens: 7, ProviderRequests: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &countingAdapter{inner: fake}
	service, err := NewService(
		resolver, adapter, newMemoryArtifactStore(), repository,
		fixedClock{now: request.GetEnvelope().GetCreatedAt().AsTime().Add(time.Minute)},
	)
	if err != nil {
		t.Fatal(err)
	}
	return service, pool, adapter
}

func TestPostgresInvocationReplayConflictAndImmutability(t *testing.T) {
	service, pool, adapter := integrationService(t)
	request := gatewayRequest(t)
	request.Envelope.RequestId = "request-" + uuid.NewString()

	first, err := service.ProposeImplementation(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.ProposeImplementation(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replay || !replay.Replay || adapter.calls != 1 ||
		!proto.Equal(first.Proposal, replay.Proposal) {
		t.Fatalf(
			"first replay=%t replay replay=%t adapter calls=%d",
			first.Replay, replay.Replay, adapter.calls,
		)
	}
	var requestURI, requestDigest, proposalURI, proposalDigest, status string
	if err := pool.QueryRow(t.Context(), `SELECT
		request_artifact_uri,request_digest,proposal_artifact_uri,proposal_digest,
		final_status
		FROM reasoning_invocations WHERE request_id=$1`,
		request.GetEnvelope().GetRequestId(),
	).Scan(
		&requestURI, &requestDigest, &proposalURI, &proposalDigest, &status,
	); err != nil {
		t.Fatal(err)
	}
	if requestURI != first.RequestArtifact.URI ||
		requestDigest != first.RequestArtifact.SHA256 ||
		proposalURI != first.ProposalArtifact.URI ||
		proposalDigest != first.ProposalArtifact.SHA256 ||
		status != string(StatusAccepted) {
		t.Fatal("persisted invocation metadata differs from outcome")
	}

	conflict := proto.Clone(request).(*reasoningv1.ImplementationRequest)
	replacement := "f"
	if conflict.GetBaseCommit()[0] == 'f' {
		replacement = "e"
	}
	conflict.BaseCommit = replacement + conflict.GetBaseCommit()[1:]
	if _, err := service.ProposeImplementation(
		t.Context(), conflict,
	); !errors.Is(err, ErrInvocationConflict) {
		t.Fatalf("conflicting request error = %v", err)
	}
	if adapter.calls != 1 {
		t.Fatal("conflicting replay invoked adapter")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE reasoning_invocations
		SET model='tampered' WHERE request_id=$1`,
		request.GetEnvelope().GetRequestId(),
	); err == nil {
		t.Fatal("completed invocation update succeeded")
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM reasoning_invocations
		WHERE request_id=$1`, request.GetEnvelope().GetRequestId(),
	); err == nil {
		t.Fatal("completed invocation delete succeeded")
	}
}

func TestPostgresInvocationReservationReleasesConnection(t *testing.T) {
	service, pool, _ := integrationService(t)
	request := gatewayRequest(t)
	request.Envelope.RequestId = "request-" + uuid.NewString()
	envelope := request.GetEnvelope()
	handle, err := service.invocations.Begin(t.Context(), InvocationStart{
		RequestID: envelope.GetRequestId(),
		RequestArtifact: ArtifactReference{
			URI:    "artifact://sha256/" + envelope.GetAgentManifestDigest(),
			SHA256: envelope.GetAgentManifestDigest(),
		},
		RunID: envelope.GetRunId(), TaskID: envelope.TaskId,
		Stage: implementationStage, Attempt: envelope.GetAttempt(),
		AgentManifestDigest: envelope.GetAgentManifestDigest(),
		StartedAt:           envelope.GetCreatedAt().AsTime(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if acquired := pool.Stat().AcquiredConns(); acquired != 0 {
		t.Fatalf("reservation retained %d database connections", acquired)
	}
	if err := handle.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestReasoningMigrationReplays(t *testing.T) {
	connectionString := os.Getenv("TEST_DATABASE_URL")
	if connectionString == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	for range 2 {
		if err := Migrate(t.Context(), connectionString); err != nil {
			t.Fatal(err)
		}
	}
}
