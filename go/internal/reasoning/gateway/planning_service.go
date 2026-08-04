package gateway

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"google.golang.org/protobuf/proto"
)

const (
	planningStage  = "planning"
	planningOutput = "task_graph_proposal.v1"
)

type PlanningAdapter interface {
	ProposeTaskGraph(
		context.Context, manifest.Manifest, *reasoningv1.TaskPlanningRequest,
	) (PlanningAdapterResult, error)
}

type PlanningAdapterResult struct {
	Proposal          *reasoningv1.TaskGraphProposal
	ProviderResponse  []byte
	ProviderRequestID string
	MalformedOutput   *MalformedOutput
	Provider          string
	Model             string
	Usage             Usage
}

type PlanningPolicy struct{ TrustedCheckIDs []string }

type PlanningOutcome struct {
	RequestArtifact          ArtifactReference
	ProposalArtifact         ArtifactReference
	ProviderResponseArtifact ArtifactReference
	Proposal                 *reasoningv1.TaskGraphProposal
	Rejection                *reasoningv1.ProposalRejection
	Invocation               InvocationMetadata
	Replay                   bool
}

type PlanningService struct {
	manifests   ManifestResolver
	adapter     PlanningAdapter
	artifacts   ArtifactStore
	invocations InvocationRepository
	clock       Clock
	limits      ByteLimits
	checks      map[string]struct{}
}

func NewPlanningService(
	manifests ManifestResolver, adapter PlanningAdapter, artifacts ArtifactStore,
	invocations InvocationRepository, clock Clock, policy PlanningPolicy,
	configuredLimits ...ByteLimits,
) (*PlanningService, error) {
	if manifests == nil || adapter == nil || artifacts == nil || invocations == nil || clock == nil {
		return nil, errors.New("planning service dependencies are required")
	}
	checks := make(map[string]struct{}, len(policy.TrustedCheckIDs))
	for _, check := range policy.TrustedCheckIDs {
		if check == "" {
			return nil, errors.New("trusted planning check IDs must be non-empty")
		}
		if _, exists := checks[check]; exists {
			return nil, errors.New("trusted planning check IDs must be unique")
		}
		checks[check] = struct{}{}
	}
	if len(checks) == 0 {
		return nil, errors.New("at least one trusted planning check is required")
	}
	limits := DefaultByteLimits()
	if len(configuredLimits) > 1 {
		return nil, errors.New("at most one byte-limit configuration is allowed")
	}
	if len(configuredLimits) == 1 {
		limits = configuredLimits[0]
	}
	if limits.Request < 1 || limits.Proposal < 1 || limits.ProviderResponse < 1 {
		return nil, errors.New("positive request, proposal, and provider-response byte limits are required")
	}
	return &PlanningService{manifests: manifests, adapter: adapter, artifacts: artifacts,
		invocations: invocations, clock: clock, limits: limits, checks: checks}, nil
}

func (s *PlanningService) ProposeTaskGraph( // skipcq: GO-R1005 -- explicit fail-closed gateway audit path
	ctx context.Context, request *reasoningv1.TaskPlanningRequest,
) (PlanningOutcome, error) {
	if err := ctx.Err(); err != nil {
		return PlanningOutcome{}, err
	}
	started := s.clock.Now().UTC()
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return PlanningOutcome{}, fmt.Errorf("serialize planning request: %w", err)
	}
	if len(requestBytes) > s.limits.Request {
		return PlanningOutcome{Rejection: planningRejection(request, "request",
			"serialized request exceeds byte limit", nil, started)}, nil
	}
	if request == nil || request.GetEnvelope() == nil || request.GetEnvelope().GetRequestId() == "" ||
		request.GetEnvelope().GetAttempt() == 0 || request.GetEnvelope().TaskId != nil {
		_, mapErr := contracts.MapTaskPlanningRequest(request)
		return PlanningOutcome{Rejection: planningRejection(request, "request",
			errorMessage(mapErr, "invalid planning request"), nil, started)}, nil
	}
	requestArtifact, err := putVerifiedArtifact(ctx, s.artifacts, requestBytes)
	if err != nil {
		return PlanningOutcome{}, fmt.Errorf("store planning request: %w", err)
	}
	envelope := request.GetEnvelope()
	handle, err := s.invocations.Begin(ctx, InvocationStart{RequestID: envelope.GetRequestId(),
		RequestArtifact: requestArtifact, RunID: envelope.GetRunId(), TaskID: envelope.TaskId,
		Stage: planningStage, Attempt: envelope.GetAttempt(),
		AgentManifestDigest: envelope.GetAgentManifestDigest(), StartedAt: started})
	if err != nil {
		return PlanningOutcome{}, fmt.Errorf("begin planning invocation: %w", err)
	}
	defer func() {
		rollbackContext, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
		defer cancel()
		_ = handle.Rollback(rollbackContext)
	}()
	if record, ok := handle.Replay(); ok {
		return s.replay(ctx, record)
	}
	mapped, mapErr := contracts.MapTaskPlanningRequest(request)
	if mapErr == nil && (request.GetTaskCountLimit() != 1 || request.GetParallelismLimit() != 1) {
		mapErr = errors.New("live planning requires one task and parallelism one")
	}
	if mapErr != nil {
		rejection := planningRejection(request, "request", mapErr.Error(), nil, started)
		record, completeErr := handle.Complete(ctx, InvocationCompletion{Provider: "gateway",
			Model: "pre-adapter", CompletedAt: started, Status: StatusRejected, Rejection: rejection})
		if completeErr != nil {
			return PlanningOutcome{}, fmt.Errorf("record planning rejection: %w", completeErr)
		}
		return planningOutcome(record, nil, false), nil
	}
	resolved, err := s.manifests.ResolveManifest(ctx, mapped.Envelope.AgentManifestDigest)
	if err != nil {
		return PlanningOutcome{}, fmt.Errorf("resolve planning manifest: %w", err)
	}
	if resolved.Digest != mapped.Envelope.AgentManifestDigest || resolved.Manifest.Stage != planningStage ||
		resolved.Manifest.Output.Schema != planningOutput {
		return PlanningOutcome{}, errors.New("resolved manifest does not match planning request")
	}
	result, err := s.adapter.ProposeTaskGraph(ctx, resolved.Manifest,
		proto.Clone(request).(*reasoningv1.TaskPlanningRequest))
	if err != nil {
		return PlanningOutcome{}, fmt.Errorf("propose task graph: %w", err)
	}
	if result.Usage.ProviderRequests < 1 || result.Usage.ProviderRequests > mapped.Envelope.MaximumRequests {
		return PlanningOutcome{}, errors.New("planning adapter violated provider request budget")
	}
	return s.finalize(ctx, handle, request, mapped, result)
}

func (s *PlanningService) finalize(
	ctx context.Context, handle InvocationHandle, request *reasoningv1.TaskPlanningRequest,
	mapped contracts.TaskPlanningRequest, result PlanningAdapterResult,
) (PlanningOutcome, error) {
	completed := s.clock.Now().UTC()
	if len(result.ProviderResponse) > s.limits.ProviderResponse {
		return PlanningOutcome{}, errors.New("provider response exceeds byte limit")
	}
	responseArtifact, err := putVerifiedArtifact(ctx, s.artifacts, result.ProviderResponse)
	if err != nil {
		return PlanningOutcome{}, fmt.Errorf("store planning provider response: %w", err)
	}
	completion := InvocationCompletion{ProviderResponseArtifact: responseArtifact,
		ProviderRequestID: result.ProviderRequestID, Provider: result.Provider, Model: result.Model,
		CompletedAt: completed, Usage: result.Usage}
	if result.MalformedOutput != nil {
		message := result.MalformedOutput.Message
		if message == "" {
			message = "provider output does not match the closed planning schema"
		}
		completion.Status = StatusRejected
		completion.Rejection = planningRejection(request, "provider_response", message,
			result.MalformedOutput, completed)
		record, completeErr := handle.Complete(ctx, completion)
		if completeErr != nil {
			return PlanningOutcome{}, fmt.Errorf("record malformed planning output: %w", completeErr)
		}
		return planningOutcome(record, nil, false), nil
	}
	trusted, policyErr := s.injectAndValidatePolicy(result.Proposal, request)
	if policyErr != nil {
		completion.Status = StatusRejected
		completion.Rejection = planningRejection(request, "proposal", policyErr.Error(), nil, completed)
		record, completeErr := handle.Complete(ctx, completion)
		if completeErr != nil {
			return PlanningOutcome{}, completeErr
		}
		return planningOutcome(record, nil, false), nil
	}
	proposalBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(trusted)
	if err != nil {
		return PlanningOutcome{}, fmt.Errorf("serialize planning proposal: %w", err)
	}
	if len(proposalBytes) > s.limits.Proposal {
		completion.Status = StatusRejected
		completion.Rejection = planningRejection(request, "proposal",
			"serialized proposal exceeds byte limit", nil, completed)
		record, completeErr := handle.Complete(ctx, completion)
		if completeErr != nil {
			return PlanningOutcome{}, completeErr
		}
		return planningOutcome(record, nil, false), nil
	}
	proposalArtifact, err := putVerifiedArtifact(ctx, s.artifacts, proposalBytes)
	if err != nil {
		return PlanningOutcome{}, fmt.Errorf("store planning proposal: %w", err)
	}
	completion.ProposalArtifact = proposalArtifact
	if _, err := contracts.MapTaskGraphProposal(trusted, mapped); err != nil {
		completion.Status = StatusRejected
		completion.Rejection = planningRejection(request, "proposal", err.Error(), nil, completed)
		record, completeErr := handle.Complete(ctx, completion)
		if completeErr != nil {
			return PlanningOutcome{}, completeErr
		}
		return planningOutcome(record, nil, false), nil
	}
	completion.Status = StatusAccepted
	record, err := handle.Complete(ctx, completion)
	if err != nil {
		return PlanningOutcome{}, fmt.Errorf("record planning proposal: %w", err)
	}
	return planningOutcome(record, trusted, false), nil
}

func (s *PlanningService) injectAndValidatePolicy(
	proposal *reasoningv1.TaskGraphProposal, request *reasoningv1.TaskPlanningRequest,
) (*reasoningv1.TaskGraphProposal, error) {
	if proposal == nil || len(proposal.GetTasks()) != 1 {
		return nil, errors.New("live planning requires exactly one task")
	}
	trusted := proto.Clone(proposal).(*reasoningv1.TaskGraphProposal)
	envelope := request.GetEnvelope()
	trusted.Identity = &reasoningv1.ProposalIdentity{SchemaVersion: envelope.GetSchemaVersion(),
		RequestId: envelope.GetRequestId(), RunId: envelope.GetRunId(), TaskId: envelope.TaskId,
		Stage: envelope.GetStage(), Attempt: envelope.GetAttempt(),
		AgentManifestDigest:  envelope.GetAgentManifestDigest(),
		InputArtifactDigests: artifactDigests(envelope.GetInputArtifacts())}
	trusted.ApprovedSpecificationId = request.GetApprovedSpecificationId()
	trusted.ApprovedSpecificationDigest = request.GetApprovedSpecificationDigest()
	task := trusted.Tasks[0]
	task.TaskId = deterministicPlanningTaskID(envelope.GetRunId())
	task.Dependencies = nil
	for _, check := range task.GetRequiredCheckIds() {
		if _, ok := s.checks[check]; !ok {
			return nil, fmt.Errorf("unknown trusted check %q", check)
		}
	}
	return trusted, nil
}

func deterministicPlanningTaskID(runID string) string {
	digest := sha256.Sum256([]byte("harness-planning-task-v1\x00" + runID))
	value := digest[:16]
	value[6] = (value[6] & 0x0f) | 0x50
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[:4], value[4:6], value[6:8],
		value[8:10], value[10:16])
}

func (s *PlanningService) replay(ctx context.Context, record InvocationRecord) (PlanningOutcome, error) {
	if err := verifyReplayProviderResponse(ctx, s.artifacts, record); err != nil {
		return PlanningOutcome{}, err
	}
	requestBytes, err := s.artifacts.Get(ctx, record.RequestArtifact)
	if err != nil || verifyArtifact(record.RequestArtifact, requestBytes) != nil {
		return PlanningOutcome{}, errors.New("load verified planning replay request")
	}
	request := &reasoningv1.TaskPlanningRequest{}
	if err := proto.Unmarshal(requestBytes, request); err != nil {
		return PlanningOutcome{}, fmt.Errorf("decode planning replay request: %w", err)
	}
	var proposal *reasoningv1.TaskGraphProposal
	if record.ProposalArtifact.URI != "" {
		body, loadErr := s.artifacts.Get(ctx, record.ProposalArtifact)
		if loadErr != nil || verifyArtifact(record.ProposalArtifact, body) != nil {
			return PlanningOutcome{}, errors.New("load verified planning replay proposal")
		}
		proposal = &reasoningv1.TaskGraphProposal{}
		if err := proto.Unmarshal(body, proposal); err != nil {
			return PlanningOutcome{}, fmt.Errorf("decode planning replay proposal: %w", err)
		}
	}
	if record.Status == StatusAccepted {
		mapped, mapErr := contracts.MapTaskPlanningRequest(request)
		if mapErr != nil {
			return PlanningOutcome{}, mapErr
		}
		if _, mapErr = contracts.MapTaskGraphProposal(proposal, mapped); mapErr != nil {
			return PlanningOutcome{}, mapErr
		}
	}
	return planningOutcome(record, proposal, true), nil
}

func planningRejection(
	request *reasoningv1.TaskPlanningRequest, field, message string,
	malformed *MalformedOutput, now time.Time,
) *reasoningv1.ProposalRejection {
	var specificationRequest *reasoningv1.SpecificationRequest
	if request != nil {
		specificationRequest = &reasoningv1.SpecificationRequest{Envelope: request.GetEnvelope()}
	}
	return specificationRejection(specificationRequest, field, message, malformed, now)
}

func planningOutcome(record InvocationRecord, proposal *reasoningv1.TaskGraphProposal, replay bool) PlanningOutcome {
	value := PlanningOutcome{RequestArtifact: record.RequestArtifact,
		ProposalArtifact: record.ProposalArtifact, ProviderResponseArtifact: record.ProviderResponseArtifact,
		Rejection: record.Rejection, Invocation: InvocationMetadata{ProviderRequestID: record.ProviderRequestID,
			Provider: record.Provider, Model: record.Model, StartedAt: record.StartedAt,
			CompletedAt: record.CompletedAt, Usage: record.Usage}, Replay: replay}
	if record.Status == StatusAccepted && proposal != nil {
		value.Proposal = proto.Clone(proposal).(*reasoningv1.TaskGraphProposal)
	}
	if value.Rejection != nil {
		value.Rejection = proto.Clone(value.Rejection).(*reasoningv1.ProposalRejection)
	}
	return value
}
