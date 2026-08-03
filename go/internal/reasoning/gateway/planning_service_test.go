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

type planningAdapterFunc func(
	context.Context, manifest.Manifest, *reasoningv1.TaskPlanningRequest,
) (PlanningAdapterResult, error)

func (f planningAdapterFunc) ProposeTaskGraph(
	ctx context.Context, value manifest.Manifest, request *reasoningv1.TaskPlanningRequest,
) (PlanningAdapterResult, error) {
	return f(ctx, value, request)
}

func planningRequestFixture(t *testing.T) *reasoningv1.TaskPlanningRequest {
	t.Helper()
	request := &reasoningv1.TaskPlanningRequest{}
	if err := proto.Unmarshal(gatewayFixture(t, "planning", "request.bin"), request); err != nil {
		t.Fatal(err)
	}
	request.TaskCountLimit, request.ParallelismLimit = 1, 1
	request.AcceptanceCriterionIds = []string{"AC-001"}
	return request
}

func planningProposalFixture(t *testing.T) *reasoningv1.TaskGraphProposal {
	t.Helper()
	proposal := &reasoningv1.TaskGraphProposal{}
	if err := proto.Unmarshal(gatewayFixture(t, "planning", "proposal.bin"), proposal); err != nil {
		t.Fatal(err)
	}
	proposal.Tasks = proposal.Tasks[:1]
	return proposal
}

func planningManifest(t *testing.T) manifest.Manifest {
	t.Helper()
	value, _, _, err := manifest.Read(gatewayFixture(t, "manifest", "planning.json"))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func planningServiceFixture(
	t *testing.T, adapter PlanningAdapter, repository InvocationRepository,
) *PlanningService {
	t.Helper()
	request := planningRequestFixture(t)
	service, err := NewPlanningService(&fakeResolver{resolved: ResolvedManifest{
		Digest: request.GetEnvelope().GetAgentManifestDigest(), Manifest: planningManifest(t)}},
		adapter, newMemoryArtifactStore(), repository, fixedClock{now: time.Now()},
		PlanningPolicy{TrustedCheckIDs: []string{"CHECK-DOCS"}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func acceptedPlanningAdapter(t *testing.T, calls *atomic.Int32) PlanningAdapter {
	t.Helper()
	return planningAdapterFunc(func(
		_ context.Context, _ manifest.Manifest, request *reasoningv1.TaskPlanningRequest,
	) (PlanningAdapterResult, error) {
		calls.Add(1)
		if request.GetEnvelope().GetAuthority().GetMayApproveWork() {
			t.Fatal("planning adapter received approval authority")
		}
		proposal := planningProposalFixture(t)
		proposal.Identity.RequestId = "provider-controlled"
		proposal.ApprovedSpecificationId = "provider-controlled"
		proposal.Tasks[0].TaskId = "TASK-999"
		proposal.Tasks[0].Dependencies = []*reasoningv1.TaskDependency{{TaskId: "TASK-999"}}
		return PlanningAdapterResult{Proposal: proposal, ProviderResponse: []byte(`{"provider":"raw"}`),
			ProviderRequestID: "req-plan", Provider: MiniMaxAnthropicProvider, Model: MiniMaxModel,
			Usage: Usage{InputTokens: 10, OutputTokens: 10, ProviderRequests: 1}}, nil
	})
}

func TestPlanningServiceInjectsOneTaskIdentityAndReplaysExactly(t *testing.T) {
	var calls atomic.Int32
	service := planningServiceFixture(t, acceptedPlanningAdapter(t, &calls), newMemoryInvocationRepository())
	request := planningRequestFixture(t)
	first, err := service.ProposeTaskGraph(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ProposeTaskGraph(t.Context(), proto.Clone(request).(*reasoningv1.TaskPlanningRequest))
	if err != nil {
		t.Fatal(err)
	}
	if first.Proposal == nil || first.Rejection != nil || first.Replay || !second.Replay || calls.Load() != 1 ||
		!proto.Equal(first.Proposal, second.Proposal) || first.ProposalArtifact != second.ProposalArtifact {
		t.Fatalf("first=%+v second=%+v calls=%d", first, second, calls.Load())
	}
	task := first.Proposal.GetTasks()[0]
	if task.GetTaskId() != deterministicPlanningTaskID(request.GetEnvelope().GetRunId()) ||
		len(task.GetDependencies()) != 0 || first.Proposal.GetApprovedSpecificationId() != request.GetApprovedSpecificationId() ||
		first.Proposal.GetIdentity().GetRequestId() != request.GetEnvelope().GetRequestId() {
		t.Fatalf("trusted planning injection failed: proposal=%+v", first.Proposal)
	}
}

func TestPlanningServiceRejectsPolicyInvalidProposals(t *testing.T) {
	tests := map[string]func(*reasoningv1.TaskGraphProposal){
		"scope widening": func(value *reasoningv1.TaskGraphProposal) {
			value.Tasks[0].WritablePaths = []string{"outside"}
		},
		"missing coverage": func(value *reasoningv1.TaskGraphProposal) {
			value.Tasks[0].AcceptanceCriterionIds = nil
		},
		"duplicate coverage": func(value *reasoningv1.TaskGraphProposal) {
			value.Tasks[0].AcceptanceCriterionIds = []string{"AC-001", "AC-001"}
		},
		"unknown check": func(value *reasoningv1.TaskGraphProposal) {
			value.Tasks[0].RequiredCheckIds = []string{"CHECK-UNKNOWN"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			adapter := planningAdapterFunc(func(
				context.Context, manifest.Manifest, *reasoningv1.TaskPlanningRequest,
			) (PlanningAdapterResult, error) {
				proposal := planningProposalFixture(t)
				mutate(proposal)
				return PlanningAdapterResult{Proposal: proposal, ProviderResponse: []byte(`{}`),
					Provider: MiniMaxAnthropicProvider, Model: MiniMaxModel,
					Usage: Usage{ProviderRequests: 1}}, nil
			})
			outcome, err := planningServiceFixture(t, adapter,
				newMemoryInvocationRepository()).ProposeTaskGraph(t.Context(), planningRequestFixture(t))
			if err != nil || outcome.Proposal != nil ||
				outcome.Rejection.GetCode() != reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID {
				t.Fatalf("outcome=%+v err=%v", outcome, err)
			}
		})
	}
}

func TestPlanningServiceMalformedAndProviderFailureAreBounded(t *testing.T) {
	malformed := planningAdapterFunc(func(
		context.Context, manifest.Manifest, *reasoningv1.TaskPlanningRequest,
	) (PlanningAdapterResult, error) {
		return PlanningAdapterResult{ProviderResponse: []byte(`{"task_id":"untrusted"}`),
			Provider: MiniMaxAnthropicProvider, Model: MiniMaxModel,
			Usage: Usage{ProviderRequests: 1}, MalformedOutput: &MalformedOutput{
				Message: "provider response is not valid planning JSON; kind=unknown_field",
				Kind:    "unknown_field", UnknownFields: []string{"task_id"}}}, nil
	})
	outcome, err := planningServiceFixture(t, malformed,
		newMemoryInvocationRepository()).ProposeTaskGraph(t.Context(), planningRequestFixture(t))
	if err != nil || outcome.Proposal != nil || len(outcome.Rejection.GetDetails()) < 2 {
		t.Fatalf("malformed outcome=%+v err=%v", outcome, err)
	}

	repository := newMemoryInvocationRepository()
	var fail atomic.Bool
	fail.Store(true)
	provider := planningAdapterFunc(func(
		context.Context, manifest.Manifest, *reasoningv1.TaskPlanningRequest,
	) (PlanningAdapterResult, error) {
		if fail.Swap(false) {
			return PlanningAdapterResult{}, errors.New("provider unavailable")
		}
		return PlanningAdapterResult{Proposal: planningProposalFixture(t), ProviderResponse: []byte(`{}`),
			Provider: MiniMaxAnthropicProvider, Model: MiniMaxModel,
			Usage: Usage{ProviderRequests: 1}}, nil
	})
	service := planningServiceFixture(t, provider, repository)
	request := planningRequestFixture(t)
	if _, err := service.ProposeTaskGraph(t.Context(), request); err == nil {
		t.Fatal("provider failure was accepted")
	}
	if retry, err := service.ProposeTaskGraph(t.Context(), request); err != nil || retry.Proposal == nil {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
}

func TestMiniMaxPlanningAdapterProjectionExcludesTaskAuthority(t *testing.T) {
	request := planningRequestFixture(t)
	request.Envelope.InputArtifacts = nil
	projection := planningProjection{Tasks: []plannedTaskProjection{{Objective: "Document contract",
		AcceptanceCriterionIDs: []string{"AC-001"}, ReadablePaths: []string{"docs"},
		WritablePaths: []string{"docs/reasoning-contracts.md"}, ProhibitedPaths: []string{},
		ExclusiveResources: []string{}, RequiredCheckIDs: []string{"CHECK-DOCS"},
		StopConditions: []string{"trusted check passes"}}}, Assumptions: []string{},
		UnresolvedScopeQuestions: []string{}}
	body, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	sender := &captureMessageSender{reply: anthropicMessage(t, string(body))}
	store := newMemoryArtifactStore()
	manifestValue := planningManifest(t)
	promptRef, err := store.Put(t.Context(), []byte("planning prompt\n"))
	if err != nil {
		t.Fatal(err)
	}
	manifestValue.Prompt.ArtifactURI, manifestValue.Prompt.SHA256 = promptRef.URI, promptRef.SHA256
	adapter, err := NewAnthropicPlanningAdapter(
		credentialSourceFunc(func(context.Context) (string, error) { return "credential", nil }),
		StaticCapabilityModels{"general_reasoning": MiniMaxModel}, store,
		WithMiniMaxCompatibility(), withAnthropicMessageSender(sender))
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.ProposeTaskGraph(t.Context(), manifestValue, request)
	if err != nil || result.Proposal == nil || len(result.Proposal.GetTasks()) != 1 ||
		result.Proposal.GetTasks()[0].GetTaskId() != "" || len(result.Proposal.GetTasks()[0].GetDependencies()) != 0 ||
		strings.Contains(sender.params.System[0].Text, `"task_id"`) ||
		strings.Contains(sender.params.System[0].Text, `"dependencies"`) {
		t.Fatalf("projection result=%+v err=%v system=%q", result, err, sender.params.System[0].Text)
	}
}
