package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"google.golang.org/protobuf/proto"
)

type specificationAdapterFunc func(
	context.Context, manifest.Manifest, *reasoningv1.SpecificationRequest,
) (SpecificationAdapterResult, error)

func (f specificationAdapterFunc) ProposeSpecification(
	ctx context.Context, value manifest.Manifest, request *reasoningv1.SpecificationRequest,
) (SpecificationAdapterResult, error) {
	return f(ctx, value, request)
}

func specificationRequestFixture(t *testing.T) *reasoningv1.SpecificationRequest {
	t.Helper()
	request := &reasoningv1.SpecificationRequest{}
	if err := proto.Unmarshal(gatewayFixture(t, "specification", "request.bin"), request); err != nil {
		t.Fatal(err)
	}
	return request
}

func specificationProposalFixture(t *testing.T) *reasoningv1.SpecificationProposal {
	t.Helper()
	proposal := &reasoningv1.SpecificationProposal{}
	if err := proto.Unmarshal(gatewayFixture(t, "specification", "proposal.bin"), proposal); err != nil {
		t.Fatal(err)
	}
	return proposal
}

func specificationManifest(t *testing.T) manifest.Manifest {
	t.Helper()
	value, _, _, err := manifest.Read(gatewayFixture(t, "manifest", "specification.json"))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestSpecificationServiceInjectsIdentityAndReplaysExactly(t *testing.T) {
	request := specificationRequestFixture(t)
	proposal := specificationProposalFixture(t)
	store := newMemoryArtifactStore()
	repository := newMemoryInvocationRepository()
	var calls atomic.Int32
	adapter := specificationAdapterFunc(func(
		_ context.Context, _ manifest.Manifest, received *reasoningv1.SpecificationRequest,
	) (SpecificationAdapterResult, error) {
		calls.Add(1)
		if received.GetEnvelope().GetAuthority().GetMayApproveWork() {
			t.Fatal("adapter received workflow authority")
		}
		untrusted := proto.Clone(proposal).(*reasoningv1.SpecificationProposal)
		untrusted.Identity.RequestId = "provider-controlled"
		return SpecificationAdapterResult{Proposal: untrusted,
			ProviderResponse: []byte(`{"provider":"raw"}`), ProviderRequestID: "req-spec",
			Provider: MiniMaxAnthropicProvider, Model: MiniMaxModel,
			Usage: Usage{InputTokens: 10, OutputTokens: 10, ProviderRequests: 1}}, nil
	})
	service, err := NewSpecificationService(&fakeResolver{resolved: ResolvedManifest{
		Digest: request.GetEnvelope().GetAgentManifestDigest(), Manifest: specificationManifest(t)}},
		adapter, store, repository, fixedClock{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.ProposeSpecification(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ProposeSpecification(t.Context(), proto.Clone(request).(*reasoningv1.SpecificationRequest))
	if err != nil {
		t.Fatal(err)
	}
	if first.Proposal == nil || first.Rejection != nil || first.Replay || !second.Replay ||
		calls.Load() != 1 || !proto.Equal(first.Proposal, second.Proposal) ||
		first.ProposalArtifact != second.ProposalArtifact ||
		first.ProviderResponseArtifact != second.ProviderResponseArtifact {
		t.Fatalf("first=%+v second=%+v calls=%d", first, second, calls.Load())
	}
	identity := first.Proposal.GetIdentity()
	if identity.GetRequestId() != request.GetEnvelope().GetRequestId() ||
		identity.GetRunId() != request.GetEnvelope().GetRunId() ||
		identity.GetStage() != reasoningv1.ReasoningStage_REASONING_STAGE_SPECIFICATION {
		t.Fatalf("proposal identity=%+v", identity)
	}
}

func TestSpecificationServiceRecordsMalformedAndEnforcesBudget(t *testing.T) {
	request := specificationRequestFixture(t)
	store := newMemoryArtifactStore()
	malformed := specificationAdapterFunc(func(
		context.Context, manifest.Manifest, *reasoningv1.SpecificationRequest,
	) (SpecificationAdapterResult, error) {
		return SpecificationAdapterResult{ProviderResponse: []byte(`{"extra":true}`),
			Provider: MiniMaxAnthropicProvider, Model: MiniMaxModel,
			Usage: Usage{ProviderRequests: 1}, MalformedOutput: &MalformedOutput{
				Message: "provider response is not valid specification JSON; kind=unknown_field",
				Kind:    "unknown_field", UnknownFields: []string{"extra"}}}, nil
	})
	service, err := NewSpecificationService(&fakeResolver{resolved: ResolvedManifest{
		Digest: request.GetEnvelope().GetAgentManifestDigest(), Manifest: specificationManifest(t)}},
		malformed, store, newMemoryInvocationRepository(), fixedClock{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := service.ProposeSpecification(t.Context(), request)
	if err != nil || outcome.Rejection.GetCode() != reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID ||
		outcome.Proposal != nil || len(outcome.Rejection.GetDetails()) < 2 {
		t.Fatalf("malformed outcome=%+v err=%v", outcome, err)
	}

	overBudget := proto.Clone(request).(*reasoningv1.SpecificationRequest)
	overBudget.Envelope.RequestId = "request-spec-budget"
	overBudget.Envelope.Budget.MaximumProviderRequests = 1
	budgetAdapter := specificationAdapterFunc(func(
		context.Context, manifest.Manifest, *reasoningv1.SpecificationRequest,
	) (SpecificationAdapterResult, error) {
		return SpecificationAdapterResult{Usage: Usage{ProviderRequests: 2}}, nil
	})
	service.adapter, service.invocations = budgetAdapter, newMemoryInvocationRepository()
	if _, err := service.ProposeSpecification(t.Context(), overBudget); err == nil ||
		!strings.Contains(err.Error(), "budget") {
		t.Fatalf("budget error=%v", err)
	}
}

func TestSpecificationServiceProviderFailureRollsBack(t *testing.T) {
	request := specificationRequestFixture(t)
	store := newMemoryArtifactStore()
	repository := newMemoryInvocationRepository()
	var fail atomic.Bool
	fail.Store(true)
	adapter := specificationAdapterFunc(func(
		context.Context, manifest.Manifest, *reasoningv1.SpecificationRequest,
	) (SpecificationAdapterResult, error) {
		if fail.Swap(false) {
			return SpecificationAdapterResult{}, errors.New("provider unavailable")
		}
		return SpecificationAdapterResult{Proposal: specificationProposalFixture(t),
			ProviderResponse: []byte(`{}`), Provider: MiniMaxAnthropicProvider, Model: MiniMaxModel,
			Usage: Usage{ProviderRequests: 1}}, nil
	})
	service, err := NewSpecificationService(&fakeResolver{resolved: ResolvedManifest{
		Digest: request.GetEnvelope().GetAgentManifestDigest(), Manifest: specificationManifest(t)}},
		adapter, store, repository, fixedClock{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ProposeSpecification(t.Context(), request); err == nil {
		t.Fatal("provider failure was accepted")
	}
	if outcome, err := service.ProposeSpecification(t.Context(), request); err != nil || outcome.Proposal == nil {
		t.Fatalf("retry outcome=%+v err=%v", outcome, err)
	}
}

func TestMiniMaxSpecificationAdapterUsesClosedProjection(t *testing.T) {
	request := specificationRequestFixture(t)
	request.Envelope.InputArtifacts = nil
	projection := specificationProjection{Title: "Bounded outcome", Goal: "Deliver it",
		Actors: []string{"operator"}, AcceptanceCriteria: []specificationCriterionProjection{{
			CriterionID: "AC-001", Description: "Observable", VerificationMethod: "trusted check"}},
		Constraints: []string{}, NonGoals: []string{}, Assumptions: []string{},
		Risks: []specificationRiskProjection{}, Questions: []specificationQuestionProjection{}}
	body, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	sender := &captureMessageSender{reply: anthropicMessage(t, string(body))}
	store := newMemoryArtifactStore()
	manifestValue := specificationManifest(t)
	promptBody := []byte(strings.TrimSpace("specification prompt") + "\n")
	promptRef, err := store.Put(t.Context(), promptBody)
	if err != nil {
		t.Fatal(err)
	}
	manifestValue.Prompt.ArtifactURI, manifestValue.Prompt.SHA256 = promptRef.URI, promptRef.SHA256
	adapter, err := NewAnthropicSpecificationAdapter(
		credentialSourceFunc(func(context.Context) (string, error) { return "credential", nil }),
		StaticCapabilityModels{"general_reasoning": MiniMaxModel}, store,
		WithMiniMaxCompatibility(), withAnthropicMessageSender(sender),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.ProposeSpecification(t.Context(), manifestValue, request)
	if err != nil || result.Proposal == nil || result.Proposal.GetIdentity().GetRequestId() != request.GetEnvelope().GetRequestId() ||
		sender.calls.Load() != 1 || sender.key != "credential" ||
		!strings.Contains(sender.params.System[0].Text, `"additionalProperties":false`) {
		t.Fatalf("adapter result=%+v calls=%d err=%v", result, sender.calls.Load(), err)
	}
}
