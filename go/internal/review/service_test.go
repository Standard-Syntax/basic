package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type memoryStore struct {
	mu     sync.Mutex
	values map[string][]byte
}

func newMemoryStore() *memoryStore { return &memoryStore{values: map[string][]byte{}} }

func (s *memoryStore) Put(_ context.Context, body []byte) (workflow.ArtifactRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	s.values[digest] = append([]byte(nil), body...)
	return workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}, nil
}

func (s *memoryStore) Get(_ context.Context, ref workflow.ArtifactRef) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.values[ref.Digest]
	if !ok {
		return nil, errors.New("artifact not found")
	}
	return append([]byte(nil), body...), nil
}

type fakeGateway struct {
	proposal *reasoningv1.ReviewProposal
	calls    int
}

func (g *fakeGateway) ProposeReview(
	_ context.Context, request *reasoningv1.ReviewRequest,
) (gateway.ReviewOutcome, error) {
	g.calls++
	proposal := proto.Clone(g.proposal).(*reasoningv1.ReviewProposal)
	proposal.Identity = &reasoningv1.ProposalIdentity{
		SchemaVersion: request.GetEnvelope().GetSchemaVersion(),
		RequestId:     request.GetEnvelope().GetRequestId(), RunId: request.GetEnvelope().GetRunId(),
		TaskId: request.GetEnvelope().TaskId, Stage: request.GetEnvelope().GetStage(),
		Attempt:             request.GetEnvelope().GetAttempt(),
		AgentManifestDigest: request.GetEnvelope().GetAgentManifestDigest(),
	}
	for _, artifact := range request.GetEnvelope().GetInputArtifacts() {
		proposal.Identity.InputArtifactDigests = append(
			proposal.Identity.InputArtifactDigests, artifact.GetSha256(),
		)
	}
	return gateway.ReviewOutcome{
		RequestArtifact: gateway.ArtifactReference{
			URI: "artifact://sha256/" + digest('1'), SHA256: digest('1'),
		},
		ProposalArtifact: gateway.ArtifactReference{
			URI: "artifact://sha256/" + digest('2'), SHA256: digest('2'),
		},
		Proposal: proposal,
	}, nil
}

type fakeWorkflow struct {
	command workflow.TaskCommand
	calls   int
}

func (w *fakeWorkflow) ExecuteTask(
	_ context.Context, command workflow.TaskCommand,
) (workflow.CommandResult, error) {
	w.calls++
	w.command = command
	return workflow.CommandResult{Revision: command.Envelope().ExpectedRevision + 1}, nil
}

func digest(value byte) string {
	return string(make([]byte, 0)) + repeat(value, 64)
}

func repeat(value byte, count int) string {
	result := make([]byte, count)
	for index := range result {
		result[index] = value
	}
	return string(result)
}

func fixture(
	t *testing.T, proposal *reasoningv1.ReviewProposal,
) (*Service, Request, *fakeGateway, *fakeWorkflow, *memoryStore) {
	t.Helper()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	output := workflow.ArtifactRef{URI: "artifact://sha256/" + digest('e'), Digest: digest('e')}
	executionReport := execution.ExecutionReport{
		SchemaVersion: "1", ExecutionID: uuid.NewString(),
		ExecutedAt: now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
		RunID:      "run-1", TaskID: "TASK-001", Attempt: 1,
		Proposal: workflow.ArtifactRef{
			URI: "artifact://sha256/" + digest('d'), Digest: digest('d'),
		},
		Lease: workflow.LeaseRef{
			ID: uuid.NewString(), OwnerID: uuid.NewString(),
			ExpiresAt: now.Add(time.Hour), FencingToken: 1,
		},
		BaseCommit: digest('a')[:40], CandidateCommit: digest('b')[:40],
		CandidateRef: "refs/harness/candidates/test", Limits: execution.DefaultLimits(),
		ActualDiff: []execution.DiffEntry{{
			Operation: contracts.FileUpdate, Path: "go/example.go",
			BeforeSHA256: digest('3'), AfterSHA256: digest('4'),
		}},
	}
	executionBody, _ := json.Marshal(executionReport)
	executionArtifact, err := store.Put(t.Context(), executionBody)
	if err != nil {
		t.Fatal(err)
	}
	started, finished := now.Add(-time.Minute), now.Add(-30*time.Second)
	verificationReport := verification.VerificationReport{
		SchemaVersion: "1", VerificationID: uuid.NewString(),
		VerifiedAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
		RunID:      "run-1", TaskID: "TASK-001", Attempt: 1, Execution: executionArtifact,
		BaseCommit: digest('a')[:40], CandidateCommit: digest('b')[:40],
		ImageID: digest('c'), Passed: true,
		Checks: []verification.CheckResult{{
			CheckID: "go-test-v1", CommandReference: "make-check", Argv: []string{"go", "test"},
			ImageID: digest('c'), CandidateCommit: digest('b')[:40],
			StartedAt:  started.Format(time.RFC3339Nano),
			FinishedAt: finished.Format(time.RFC3339Nano),
			ExitCode:   0, Output: output, OutputDigest: output.Digest, Passed: true,
		}},
		Coverage: []verification.CriterionCoverage{{
			CriterionID: "AC-001", CheckIDs: []string{"go-test-v1"}, Covered: true, Passed: true,
		}},
	}
	verificationBody, _ := json.Marshal(verificationReport)
	verificationArtifact, err := store.Put(t.Context(), verificationBody)
	if err != nil {
		t.Fatal(err)
	}
	taskID := "TASK-001"
	reviewRequest := &reasoningv1.ReviewRequest{
		Envelope: &reasoningv1.ReasoningRequestEnvelope{
			SchemaVersion: "1", RequestId: "review-request-1", RunId: "run-1",
			TaskId: &taskID, Stage: reasoningv1.ReasoningStage_REASONING_STAGE_REVIEW,
			Attempt: 1, CreatedAt: timestamppb.New(now.Add(-time.Minute)),
			ExpiresAt: timestamppb.New(now.Add(time.Hour)),
			Budget: &reasoningv1.ReasoningBudget{
				MaximumInputTokens: 10, MaximumOutputTokens: 10, MaximumProviderRequests: 1,
			},
			Authority: &reasoningv1.AuthorityConstraints{
				Mode: reasoningv1.AuthorityMode_AUTHORITY_MODE_PROPOSAL_ONLY,
			},
			AgentManifestDigest: digest('f'),
			InputArtifacts: []*reasoningv1.ArtifactDigest{
				{ArtifactUri: "artifact://sha256/" + digest('d'), Sha256: digest('d')},
				{ArtifactUri: executionArtifact.URI, Sha256: executionArtifact.Digest},
				{ArtifactUri: verificationArtifact.URI, Sha256: verificationArtifact.Digest},
			},
		},
		Candidate: &reasoningv1.ReviewCandidateIdentity{
			ApprovedSpecificationDigest: digest('5'), ApprovedTaskDigest: digest('6'),
			BaseCommit: digest('a')[:40], CandidateCommit: digest('b')[:40],
			ImplementationProposalDigest: digest('d'),
		},
		ActualDiff: []*reasoningv1.ActualDiffFile{{
			Path: "go/example.go", Operation: reasoningv1.FileOperation_FILE_OPERATION_UPDATE,
			BeforeSha256: digest('3'), AfterSha256: digest('4'),
		}},
		ScopeReport: &reasoningv1.ScopeReport{
			AuthorizedChangedPaths: []string{"go/example.go"},
		},
		IndependentEvidence: []*reasoningv1.IndependentEvidence{{
			EvidenceId: "EVIDENCE-001", CheckId: "go-test-v1",
			CandidateCommit: digest('b')[:40], ExitCode: 0,
			OutputSha256: output.Digest, ArtifactUri: output.URI,
			StartedAt: timestamppb.New(started), CompletedAt: timestamppb.New(finished),
		}},
		AcceptanceCoverage: []*reasoningv1.AcceptanceEvidence{{
			AcceptanceCriterionId: "AC-001", EvidenceIds: []string{"EVIDENCE-001"},
		}},
		ReviewPolicy: &reasoningv1.ReviewPolicy{
			BlockingSeverities: []reasoningv1.FindingSeverity{
				reasoningv1.FindingSeverity_FINDING_SEVERITY_HIGH,
				reasoningv1.FindingSeverity_FINDING_SEVERITY_CRITICAL,
			},
			ReportUnrequestedChanges: true,
		},
		ApprovedAcceptanceCriterionIds: []string{"AC-001"},
		AuthorizedWritablePaths:        []string{"go"},
	}
	gatewayPort := &fakeGateway{proposal: proposal}
	workflowPort := &fakeWorkflow{}
	service, err := NewService(
		Config{ActorID: uuid.NewString()}, store, gatewayPort, workflowPort,
	)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	return service, Request{
		ReviewID: uuid.NewString(), ReviewTimestamp: now, Review: reviewRequest,
		ExecutionArtifact: executionArtifact, VerificationArtifact: verificationArtifact,
		ExclusiveResourceLabels: []string{"public-api-contract"}, ExpectedTaskRevision: 7,
	}, gatewayPort, workflowPort, store
}

func TestReviewRecordsAdvisoryOnlyAtAwaitingApprovalBoundary(t *testing.T) {
	proposal := &reasoningv1.ReviewProposal{
		Recommendation: reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_ADVISORY_ACCEPT,
	}
	service, request, gatewayPort, workflowPort, store := fixture(t, proposal)
	result, err := service.Review(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	command, ok := workflowPort.command.(workflow.RecordTaskReview)
	if !ok || !command.Passed || command.Meta.Actor.Kind != workflow.ActorReviewService ||
		result.Recommendation != reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_ADVISORY_ACCEPT ||
		gatewayPort.calls != 1 || workflowPort.calls != 1 {
		t.Fatalf("result=%#v command=%#v", result, workflowPort.command)
	}
	body, err := store.Get(t.Context(), result.ReportArtifact)
	if err != nil {
		t.Fatal(err)
	}
	var report ReviewReport
	if err := json.Unmarshal(body, &report); err != nil || !report.Passed ||
		report.CandidateCommit != request.Review.GetCandidate().GetCandidateCommit() {
		t.Fatalf("report=%#v err=%v", report, err)
	}
}

func TestHighFindingDeterministicallyRecordsRework(t *testing.T) {
	proposal := &reasoningv1.ReviewProposal{
		Recommendation: reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_REWORK_REQUIRED,
		Findings: []*reasoningv1.ReviewFinding{{
			FindingId: "FINDING-1",
			Severity:  reasoningv1.FindingSeverity_FINDING_SEVERITY_HIGH,
			Category:  reasoningv1.FindingCategory_FINDING_CATEGORY_CORRECTNESS,
			Summary:   "defect", EvidenceReferences: []string{"EVIDENCE-001"},
		}},
		RequiredActions: []*reasoningv1.RequiredAction{{
			ActionId: "ACTION-1", FindingId: "FINDING-1", Description: "fix it",
		}},
	}
	service, request, _, workflowPort, _ := fixture(t, proposal)
	result, err := service.Review(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	command := workflowPort.command.(workflow.RecordTaskReview)
	if result.Passed || command.Passed {
		t.Fatal("blocking review advanced toward approval")
	}
}

func TestForgedExecutionEvidenceCausesNoReviewOrTransition(t *testing.T) {
	proposal := &reasoningv1.ReviewProposal{
		Recommendation: reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_ADVISORY_ACCEPT,
	}
	service, request, gatewayPort, workflowPort, store := fixture(t, proposal)
	body, err := store.Get(t.Context(), request.ExecutionArtifact)
	if err != nil {
		t.Fatal(err)
	}
	body[0] ^= 1
	store.values[request.ExecutionArtifact.Digest] = body
	if _, err := service.Review(t.Context(), request); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("forged evidence err=%v", err)
	}
	if gatewayPort.calls != 0 || workflowPort.calls != 0 {
		t.Fatal("forged evidence reached reviewer or workflow")
	}
}
