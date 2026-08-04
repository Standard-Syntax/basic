//go:build integration

package gateway

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
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
		Digest: request.GetEnvelope().GetAgentManifestDigest(),
	}}
	store := newMemoryArtifactStore()
	seedImplementationAdapterArtifacts(t, store, request)
	agentManifest := implementationManifest(t)
	prompt, err := store.Put(t.Context(), []byte(testPromptBody))
	if err != nil {
		t.Fatal(err)
	}
	agentManifest.Prompt.ArtifactURI = prompt.URI
	agentManifest.Prompt.SHA256 = prompt.SHA256
	resolver.resolved.Manifest = agentManifest
	production, provider := newLoopbackImplementationAdapter(t, store, gatewayProposal(t))
	adapter := &countingAdapter{inner: production, provider: provider}
	service, err := NewService(
		resolver, adapter, store, repository,
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
	if first.Replay || !replay.Replay || adapter.calls.Load() != 1 ||
		!proto.Equal(first.Proposal, replay.Proposal) {
		t.Fatalf(
			"first replay=%t replay replay=%t adapter calls=%d",
			first.Replay, replay.Replay, adapter.calls.Load(),
		)
	}
	var (
		requestURI, requestDigest, proposalURI, proposalDigest string
		responseURI, responseDigest, status, model             string
		inputTokens, outputTokens                              uint64
		providerRequests                                       uint32
	)
	if err := pool.QueryRow(t.Context(), `SELECT
		request_artifact_uri,request_digest,proposal_artifact_uri,proposal_digest,
		provider_response_artifact_uri,provider_response_digest,final_status,model,
		input_tokens,output_tokens,provider_requests
		FROM reasoning_invocations WHERE request_id=$1`,
		request.GetEnvelope().GetRequestId(),
	).Scan(
		&requestURI, &requestDigest, &proposalURI, &proposalDigest,
		&responseURI, &responseDigest, &status, &model,
		&inputTokens, &outputTokens, &providerRequests,
	); err != nil {
		t.Fatal(err)
	}
	if requestURI != first.RequestArtifact.URI ||
		requestDigest != first.RequestArtifact.SHA256 ||
		proposalURI != first.ProposalArtifact.URI ||
		proposalDigest != first.ProposalArtifact.SHA256 ||
		responseURI != first.ProviderResponseArtifact.URI ||
		responseDigest != first.ProviderResponseArtifact.SHA256 ||
		status != string(StatusAccepted) || model != first.Invocation.Model ||
		inputTokens != first.Invocation.Usage.InputTokens ||
		outputTokens != first.Invocation.Usage.OutputTokens ||
		providerRequests != first.Invocation.Usage.ProviderRequests {
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
	if adapter.calls.Load() != 1 {
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
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- Migrate(context.Background(), connectionString) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	pool, err := pgxpool.New(t.Context(), connectionString)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var original string
	if err := pool.QueryRow(t.Context(),
		`SELECT digest FROM schema_migrations WHERE version=7`,
	).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE schema_migrations
		SET digest=repeat('0',64) WHERE version=7`); err != nil {
		t.Fatal(err)
	}
	migrateErr := Migrate(t.Context(), connectionString)
	if _, err := pool.Exec(t.Context(), `UPDATE schema_migrations
		SET digest=$1 WHERE version=7`, original); err != nil {
		t.Fatal(err)
	}
	if migrateErr == nil {
		t.Fatal("changed reasoning migration digest was accepted")
	}
	body, err := reasoningMigrationFiles.ReadFile(
		"migrations/0007_reasoning_invocations.sql",
	)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if original != fmt.Sprintf("%x", sum) {
		t.Fatal("reasoning migration ledger digest differs from embedded SQL")
	}
	var providerOutcomeMigration int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM schema_migrations WHERE version=16`,
	).Scan(&providerOutcomeMigration); err != nil {
		t.Fatal(err)
	}
	if providerOutcomeMigration != 1 {
		t.Fatal("provider outcome migration was not recorded exactly once")
	}
	var livePlanningValidationMigration, validatedConstraints int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM schema_migrations WHERE version=25`,
	).Scan(&livePlanningValidationMigration); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint
		WHERE conname IN ('reasoning_invocations_stage_check',
		'reasoning_invocations_specification_task_check',
		'reasoning_invocations_planning_task_check') AND convalidated`,
	).Scan(&validatedConstraints); err != nil {
		t.Fatal(err)
	}
	if livePlanningValidationMigration != 1 || validatedConstraints != 3 {
		t.Fatalf("live planning migration=%d validated constraints=%d",
			livePlanningValidationMigration, validatedConstraints)
	}
}

func TestPostgresConcurrentIdenticalAndConflictingRequests(t *testing.T) {
	t.Run("identical", func(t *testing.T) {
		service, _, adapter := integrationService(t)
		request := gatewayRequest(t)
		request.Envelope.RequestId = "request-" + uuid.NewString()
		const calls = 10
		outcomes := make(chan Outcome, calls)
		errs := make(chan error, calls)
		for range calls {
			go func() {
				outcome, err := service.ProposeImplementation(
					context.Background(),
					proto.Clone(request).(*reasoningv1.ImplementationRequest),
				)
				outcomes <- outcome
				errs <- err
			}()
		}
		replays := 0
		for range calls {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
			if (<-outcomes).Replay {
				replays++
			}
		}
		if adapter.calls.Load() != 1 || replays != calls-1 {
			t.Fatalf("adapter calls=%d replays=%d", adapter.calls.Load(), replays)
		}
	})
	t.Run("conflicting", func(t *testing.T) {
		service, _, adapter := integrationService(t)
		first := gatewayRequest(t)
		first.Envelope.RequestId = "request-" + uuid.NewString()
		second := proto.Clone(first).(*reasoningv1.ImplementationRequest)
		replacement := "f"
		if second.GetBaseCommit()[0] == 'f' {
			replacement = "e"
		}
		second.BaseCommit = replacement + second.GetBaseCommit()[1:]
		errs := make(chan error, 2)
		for _, request := range []*reasoningv1.ImplementationRequest{first, second} {
			go func() {
				_, err := service.ProposeImplementation(context.Background(), request)
				errs <- err
			}()
		}
		var successes, conflicts int
		for range 2 {
			err := <-errs
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrInvocationConflict):
				conflicts++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if successes != 1 || conflicts != 1 || adapter.calls.Load() != 1 {
			t.Fatalf(
				"successes=%d conflicts=%d adapter calls=%d",
				successes, conflicts, adapter.calls.Load(),
			)
		}
	})
}

func TestPostgresInvocationRollbackAtFailurePoints(t *testing.T) {
	for _, point := range []RepositoryFaultPoint{
		FaultBeforeInvocationInsert, FaultBeforeInvocationCommit,
	} {
		t.Run(string(point), func(t *testing.T) {
			service, pool, adapter := integrationService(t)
			repository := service.invocations.(*PostgresInvocationRepository)
			repository.inject = func(actual RepositoryFaultPoint) error {
				if actual == point {
					return errors.New("injected repository failure")
				}
				return nil
			}
			request := gatewayRequest(t)
			request.Envelope.RequestId = "request-" + uuid.NewString()
			if _, err := service.ProposeImplementation(
				t.Context(), request,
			); err == nil {
				t.Fatal("injected repository failure was ignored")
			}
			var count int
			if err := pool.QueryRow(t.Context(), `SELECT count(*)
				FROM reasoning_invocations WHERE request_id=$1`,
				request.GetEnvelope().GetRequestId(),
			).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("failed transaction retained an invocation row")
			}
			repository.inject = nil
			outcome, err := service.ProposeImplementation(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Replay || adapter.calls.Load() != 2 {
				t.Fatalf(
					"retry replay=%t adapter calls=%d",
					outcome.Replay, adapter.calls.Load(),
				)
			}
		})
	}
}

func TestPostgresAllRejectionsHaveNoWorkflowOrRepositorySideEffects(t *testing.T) {
	connectionString := os.Getenv("TEST_DATABASE_URL")
	if connectionString == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if err := workflow.Migrate(t.Context(), connectionString); err != nil {
		t.Fatal(err)
	}
	before := reasoningFiles(t)
	tests := []struct {
		name   string
		code   reasoningv1.RejectionCode
		mutate func(*Service, *countingAdapter, *reasoningv1.ImplementationRequest)
	}{
		{
			name: "schema invalid",
			code: reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			mutate: func(_ *Service, adapter *countingAdapter, _ *reasoningv1.ImplementationRequest) {
				proposal := gatewayProposal(t)
				proposal.Summary = ""
				adapter.provider.setResponse(implementationProjectionJSON(t, proposal))
			},
		},
		{
			name: "request mismatch",
			code: reasoningv1.RejectionCode_REJECTION_CODE_REQUEST_MISMATCH,
			mutate: func(service *Service, _ *countingAdapter, request *reasoningv1.ImplementationRequest) {
				service.clock = fixedClock{
					now: request.GetEnvelope().GetExpiresAt().AsTime().Add(time.Second),
				}
			},
		},
		{
			name: "authority violation",
			code: reasoningv1.RejectionCode_REJECTION_CODE_AUTHORITY_VIOLATION,
			mutate: func(_ *Service, _ *countingAdapter, request *reasoningv1.ImplementationRequest) {
				request.Envelope.Authority.MayApproveWork = true
			},
		},
		{
			name: "scope violation",
			code: reasoningv1.RejectionCode_REJECTION_CODE_SCOPE_VIOLATION,
			mutate: func(_ *Service, adapter *countingAdapter, _ *reasoningv1.ImplementationRequest) {
				proposal := gatewayProposal(t)
				proposal.Changes[0].Path = "docs/outside.go"
				adapter.provider.setResponse(implementationProjectionJSON(t, proposal))
			},
		},
		{
			name: "required coverage missing",
			code: reasoningv1.RejectionCode_REJECTION_CODE_REQUIRED_COVERAGE_MISSING,
			mutate: func(_ *Service, adapter *countingAdapter, _ *reasoningv1.ImplementationRequest) {
				proposal := gatewayProposal(t)
				proposal.Changes = proposal.Changes[:2]
				adapter.provider.setResponse(implementationProjectionJSON(t, proposal))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, pool, adapter := integrationService(t)
			request := gatewayRequest(t)
			request.Envelope.RequestId = "request-" + uuid.NewString()
			request.Envelope.RunId = uuid.NewString()
			test.mutate(service, adapter, request)
			outcome, err := service.ProposeImplementation(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Proposal != nil || outcome.Rejection.GetCode() != test.code {
				t.Fatalf("outcome = %+v", outcome)
			}
			var status string
			var rejectionCode int32
			if err := pool.QueryRow(t.Context(), `SELECT final_status,rejection_code
				FROM reasoning_invocations WHERE request_id=$1`,
				request.GetEnvelope().GetRequestId(),
			).Scan(&status, &rejectionCode); err != nil {
				t.Fatal(err)
			}
			if status != string(StatusRejected) ||
				rejectionCode != int32(test.code) {
				t.Fatalf("status=%s rejection code=%d", status, rejectionCode)
			}
			assertNoWorkflowRows(t, pool, request.GetEnvelope().GetRunId())
		})
	}
	after := reasoningFiles(t)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rejected proposals changed repository files")
	}
}

func assertNoWorkflowRows(t *testing.T, pool *pgxpool.Pool, runID string) {
	t.Helper()
	queries := []string{
		`SELECT count(*) FROM workflow_runs WHERE run_id=$1`,
		`SELECT count(*) FROM workflow_tasks WHERE run_id=$1`,
		`SELECT count(*) FROM workflow_task_dependencies WHERE run_id=$1`,
		`SELECT count(*) FROM workflow_commands WHERE aggregate_id=$1`,
		`SELECT count(*) FROM workflow_events WHERE aggregate_id=$1`,
	}
	for _, query := range queries {
		var count int
		if err := pool.QueryRow(t.Context(), query, runID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("workflow side effect for run %s: %s", runID, query)
		}
	}
}

func reasoningFiles(t *testing.T) map[string][sha256.Size]byte {
	t.Helper()
	result := make(map[string][sha256.Size]byte)
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[path] = sha256.Sum256(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
