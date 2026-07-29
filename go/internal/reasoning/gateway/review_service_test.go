package gateway

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"google.golang.org/protobuf/proto"
)

func reviewRequestFixture(t *testing.T) *reasoningv1.ReviewRequest {
	t.Helper()
	var request reasoningv1.ReviewRequest
	if err := proto.Unmarshal(gatewayFixture(t, "review", "request.bin"), &request); err != nil {
		t.Fatal(err)
	}
	return &request
}

func reviewProposalFixture(t *testing.T) *reasoningv1.ReviewProposal {
	t.Helper()
	var proposal reasoningv1.ReviewProposal
	if err := proto.Unmarshal(gatewayFixture(t, "review", "proposal.bin"), &proposal); err != nil {
		t.Fatal(err)
	}
	return &proposal
}

func reviewManifest(t *testing.T) manifest.Manifest {
	t.Helper()
	value, _, _, err := manifest.Read(gatewayFixture(t, "manifest", "review.json"))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

type countingReviewAdapter struct {
	inner ReviewAdapter
	calls atomic.Int32
}

func (a *countingReviewAdapter) ProposeReview(
	ctx context.Context, value manifest.Manifest, request *reasoningv1.ReviewRequest,
) (ReviewAdapterResult, error) {
	a.calls.Add(1)
	return a.inner.ProposeReview(ctx, value, request)
}

func reviewGatewayService(
	t *testing.T, proposal *reasoningv1.ReviewProposal,
) (*ReviewService, *countingReviewAdapter) {
	t.Helper()
	request := reviewRequestFixture(t)
	resolver := &fakeResolver{resolved: ResolvedManifest{
		Digest: request.GetEnvelope().GetAgentManifestDigest(), Manifest: reviewManifest(t),
	}}
	fake, err := NewFakeReviewAdapter(
		proposal, "fake-review-v1",
		Usage{InputTokens: 13, OutputTokens: 5, ProviderRequests: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &countingReviewAdapter{inner: fake}
	service, err := NewReviewService(
		resolver, adapter, newMemoryArtifactStore(), newMemoryInvocationRepository(),
		fixedClock{now: request.GetEnvelope().GetCreatedAt().AsTime().Add(time.Minute)},
	)
	if err != nil {
		t.Fatal(err)
	}
	return service, adapter
}

func TestFakeReviewReturnsDeterministicAdvisoryAndExactReplay(t *testing.T) {
	service, adapter := reviewGatewayService(t, reviewProposalFixture(t))
	request := reviewRequestFixture(t)
	first, err := service.ProposeReview(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ProposeReview(t.Context(), proto.Clone(request).(*reasoningv1.ReviewRequest))
	if err != nil {
		t.Fatal(err)
	}
	if first.Proposal == nil || first.Rejection != nil || first.Replay ||
		!second.Replay || adapter.calls.Load() != 1 ||
		!proto.Equal(first.Proposal, second.Proposal) ||
		first.ProposalArtifact != second.ProposalArtifact {
		t.Fatalf("unexpected review/replay: first=%#v second=%#v calls=%d", first, second, adapter.calls.Load())
	}
	if first.Proposal.GetIdentity().GetRequestId() != request.GetEnvelope().GetRequestId() ||
		first.Proposal.GetIdentity().GetStage() != reasoningv1.ReasoningStage_REASONING_STAGE_REVIEW ||
		request.GetEnvelope().GetAuthority().GetMayApproveWork() {
		t.Fatal("review proposal was not request-bound or gained approval authority")
	}
}

func TestFakeReviewPreservesBlockingRework(t *testing.T) {
	proposal := reviewProposalFixture(t)
	proposal.Recommendation = reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_REWORK_REQUIRED
	if len(proposal.Findings) == 0 {
		proposal.Findings = []*reasoningv1.ReviewFinding{{
			FindingId:          "FINDING-001",
			Severity:           reasoningv1.FindingSeverity_FINDING_SEVERITY_HIGH,
			Category:           reasoningv1.FindingCategory_FINDING_CATEGORY_CORRECTNESS,
			Summary:            "blocking correctness defect",
			EvidenceReferences: []string{"EVIDENCE-001"},
		}}
		proposal.RequiredActions = []*reasoningv1.RequiredAction{{
			ActionId: "ACTION-001", FindingId: "FINDING-001", Description: "fix defect",
		}}
	} else {
		proposal.Findings[0].Severity = reasoningv1.FindingSeverity_FINDING_SEVERITY_HIGH
	}
	service, _ := reviewGatewayService(t, proposal)
	outcome, err := service.ProposeReview(t.Context(), reviewRequestFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Proposal == nil ||
		outcome.Proposal.GetRecommendation() !=
			reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_REWORK_REQUIRED {
		t.Fatalf("blocking review was not preserved: %#v", outcome)
	}
}

func TestReviewGatewayReturnsAllStableRejectionCodes(t *testing.T) {
	tests := []struct {
		name string
		code reasoningv1.RejectionCode
		edit func(*reasoningv1.ReviewRequest)
	}{
		{"schema", reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			func(r *reasoningv1.ReviewRequest) { r.ScopeReport = nil }},
		{"mismatch", reasoningv1.RejectionCode_REJECTION_CODE_REQUEST_MISMATCH,
			func(r *reasoningv1.ReviewRequest) { r.Candidate.ApprovedTaskDigest = "bad" }},
		{"authority", reasoningv1.RejectionCode_REJECTION_CODE_AUTHORITY_VIOLATION,
			func(r *reasoningv1.ReviewRequest) { r.Envelope.Authority.MayApproveWork = true }},
		{"scope", reasoningv1.RejectionCode_REJECTION_CODE_SCOPE_VIOLATION,
			func(r *reasoningv1.ReviewRequest) { r.ActualDiff[0].Path = "../escape" }},
		{"coverage", reasoningv1.RejectionCode_REJECTION_CODE_REQUIRED_COVERAGE_MISSING,
			func(r *reasoningv1.ReviewRequest) { r.AcceptanceCoverage = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, adapter := reviewGatewayService(t, reviewProposalFixture(t))
			request := reviewRequestFixture(t)
			test.edit(request)
			outcome, err := service.ProposeReview(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Rejection.GetCode() != test.code || adapter.calls.Load() != 0 {
				t.Fatalf("rejection=%v calls=%d", outcome.Rejection, adapter.calls.Load())
			}
		})
	}
}

func TestReviewGatewayRejectsConflictingRequestIDReuse(t *testing.T) {
	service, _ := reviewGatewayService(t, reviewProposalFixture(t))
	request := reviewRequestFixture(t)
	if _, err := service.ProposeReview(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	conflict := proto.Clone(request).(*reasoningv1.ReviewRequest)
	conflict.Candidate.CandidateCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, err := service.ProposeReview(t.Context(), conflict)
	if !errors.Is(err, ErrInvocationConflict) {
		t.Fatalf("conflicting request ID err=%v", err)
	}
}

func TestNewReviewServiceValidatesDependenciesAndLimits(t *testing.T) {
	fake, err := NewFakeReviewAdapter(
		reviewProposalFixture(t), "fake", Usage{ProviderRequests: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewReviewService(nil, fake, newMemoryArtifactStore(),
		newMemoryInvocationRepository(), fixedClock{now: time.Now()})
	if err == nil {
		t.Fatal("nil manifest resolver accepted")
	}
}
