package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	specificationStage  = "specification"
	specificationOutput = "specification_proposal.v1"
)

type SpecificationAdapter interface {
	ProposeSpecification(
		context.Context, manifest.Manifest, *reasoningv1.SpecificationRequest,
	) (SpecificationAdapterResult, error)
}

type SpecificationAdapterResult struct {
	Proposal          *reasoningv1.SpecificationProposal
	ProviderResponse  []byte
	ProviderRequestID string
	MalformedOutput   *MalformedOutput
	Provider          string
	Model             string
	Usage             Usage
}

type SpecificationOutcome struct {
	RequestArtifact          ArtifactReference
	ProposalArtifact         ArtifactReference
	ProviderResponseArtifact ArtifactReference
	Proposal                 *reasoningv1.SpecificationProposal
	Rejection                *reasoningv1.ProposalRejection
	Invocation               InvocationMetadata
	Replay                   bool
}

type SpecificationService struct {
	manifests   ManifestResolver
	adapter     SpecificationAdapter
	artifacts   ArtifactStore
	invocations InvocationRepository
	clock       Clock
	limits      ByteLimits
}

func NewSpecificationService(
	manifests ManifestResolver, adapter SpecificationAdapter, artifacts ArtifactStore,
	invocations InvocationRepository, clock Clock, configuredLimits ...ByteLimits,
) (*SpecificationService, error) {
	if manifests == nil || adapter == nil || artifacts == nil || invocations == nil || clock == nil {
		return nil, errors.New("specification service dependencies are required")
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
	return &SpecificationService{manifests: manifests, adapter: adapter, artifacts: artifacts,
		invocations: invocations, clock: clock, limits: limits}, nil
}

func (s *SpecificationService) ProposeSpecification( // skipcq: GO-R1005 -- explicit fail-closed gateway audit path
	ctx context.Context, request *reasoningv1.SpecificationRequest,
) (SpecificationOutcome, error) {
	if err := ctx.Err(); err != nil {
		return SpecificationOutcome{}, err
	}
	started := s.clock.Now().UTC()
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return SpecificationOutcome{}, fmt.Errorf("serialize specification request: %w", err)
	}
	if len(requestBytes) > s.limits.Request {
		return SpecificationOutcome{Rejection: specificationRejection(request,
			"request", "serialized request exceeds byte limit", nil, started)}, nil
	}
	if request == nil || request.GetEnvelope() == nil || request.GetEnvelope().GetRequestId() == "" ||
		request.GetEnvelope().GetAttempt() == 0 || request.GetEnvelope().TaskId != nil {
		_, mapErr := contracts.MapSpecificationRequest(request)
		return SpecificationOutcome{Rejection: specificationRejection(request,
			"request", errorMessage(mapErr, "invalid specification request"), nil, started)}, nil
	}
	requestArtifact, err := putVerifiedArtifact(ctx, s.artifacts, requestBytes)
	if err != nil {
		return SpecificationOutcome{}, fmt.Errorf("store specification request: %w", err)
	}
	envelope := request.GetEnvelope()
	handle, err := s.invocations.Begin(ctx, InvocationStart{
		RequestID: envelope.GetRequestId(), RequestArtifact: requestArtifact,
		RunID: envelope.GetRunId(), TaskID: envelope.TaskId, Stage: specificationStage,
		Attempt: envelope.GetAttempt(), AgentManifestDigest: envelope.GetAgentManifestDigest(),
		StartedAt: started,
	})
	if err != nil {
		return SpecificationOutcome{}, fmt.Errorf("begin specification invocation: %w", err)
	}
	defer func() {
		rollbackContext, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
		defer cancel()
		_ = handle.Rollback(rollbackContext)
	}()
	if record, ok := handle.Replay(); ok {
		return s.replay(ctx, record)
	}
	mapped, err := contracts.MapSpecificationRequest(request)
	if err != nil {
		rejection := specificationRejection(request, "request", err.Error(), nil, started)
		record, completeErr := handle.Complete(ctx, InvocationCompletion{
			Provider: "gateway", Model: "pre-adapter", CompletedAt: started,
			Status: StatusRejected, Rejection: rejection,
		})
		if completeErr != nil {
			return SpecificationOutcome{}, fmt.Errorf("record specification rejection: %w", completeErr)
		}
		return specificationOutcome(record, nil, false), nil
	}
	resolved, err := s.manifests.ResolveManifest(ctx, mapped.Envelope.AgentManifestDigest)
	if err != nil {
		return SpecificationOutcome{}, fmt.Errorf("resolve specification manifest: %w", err)
	}
	if resolved.Digest != mapped.Envelope.AgentManifestDigest ||
		resolved.Manifest.Stage != specificationStage || resolved.Manifest.Output.Schema != specificationOutput {
		return SpecificationOutcome{}, errors.New("resolved manifest does not match specification request")
	}
	result, err := s.adapter.ProposeSpecification(ctx, resolved.Manifest,
		proto.Clone(request).(*reasoningv1.SpecificationRequest))
	if err != nil {
		return SpecificationOutcome{}, fmt.Errorf("propose specification: %w", err)
	}
	if result.Usage.ProviderRequests < 1 || result.Usage.ProviderRequests > mapped.Envelope.MaximumRequests {
		return SpecificationOutcome{}, errors.New("specification adapter violated provider request budget")
	}
	return s.finalize(ctx, handle, request, mapped, result)
}

func (s *SpecificationService) finalize(
	ctx context.Context, handle InvocationHandle, request *reasoningv1.SpecificationRequest,
	mapped contracts.SpecificationRequest, result SpecificationAdapterResult,
) (SpecificationOutcome, error) {
	completed := s.clock.Now().UTC()
	if len(result.ProviderResponse) > s.limits.ProviderResponse {
		return SpecificationOutcome{}, errors.New("provider response exceeds byte limit")
	}
	responseArtifact, err := putVerifiedArtifact(ctx, s.artifacts, result.ProviderResponse)
	if err != nil {
		return SpecificationOutcome{}, fmt.Errorf("store specification provider response: %w", err)
	}
	completion := InvocationCompletion{ProviderResponseArtifact: responseArtifact,
		ProviderRequestID: result.ProviderRequestID, Provider: result.Provider, Model: result.Model,
		CompletedAt: completed, Usage: result.Usage}
	if result.MalformedOutput != nil {
		message := result.MalformedOutput.Message
		if message == "" {
			message = "provider output does not match the closed specification schema"
		}
		completion.Status = StatusRejected
		completion.Rejection = specificationRejection(request, "provider_response", message,
			result.MalformedOutput, completed)
		record, completeErr := handle.Complete(ctx, completion)
		if completeErr != nil {
			return SpecificationOutcome{}, fmt.Errorf("record malformed specification output: %w", completeErr)
		}
		return specificationOutcome(record, nil, false), nil
	}
	result.Proposal = injectSpecificationIdentity(result.Proposal, request)
	proposalBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(result.Proposal)
	if err != nil {
		return SpecificationOutcome{}, fmt.Errorf("serialize specification proposal: %w", err)
	}
	if len(proposalBytes) > s.limits.Proposal {
		completion.Status = StatusRejected
		completion.Rejection = specificationRejection(request, "proposal",
			"serialized proposal exceeds byte limit", nil, completed)
		record, completeErr := handle.Complete(ctx, completion)
		if completeErr != nil {
			return SpecificationOutcome{}, completeErr
		}
		return specificationOutcome(record, nil, false), nil
	}
	proposalArtifact, err := putVerifiedArtifact(ctx, s.artifacts, proposalBytes)
	if err != nil {
		return SpecificationOutcome{}, fmt.Errorf("store specification proposal: %w", err)
	}
	completion.ProposalArtifact = proposalArtifact
	if _, err := contracts.MapSpecificationProposal(result.Proposal, mapped); err != nil {
		completion.Status = StatusRejected
		completion.Rejection = specificationRejection(request, "proposal", err.Error(), nil, completed)
		record, completeErr := handle.Complete(ctx, completion)
		if completeErr != nil {
			return SpecificationOutcome{}, completeErr
		}
		return specificationOutcome(record, nil, false), nil
	}
	completion.Status = StatusAccepted
	record, err := handle.Complete(ctx, completion)
	if err != nil {
		return SpecificationOutcome{}, fmt.Errorf("record specification proposal: %w", err)
	}
	return specificationOutcome(record, result.Proposal, false), nil
}

func injectSpecificationIdentity(
	proposal *reasoningv1.SpecificationProposal, request *reasoningv1.SpecificationRequest,
) *reasoningv1.SpecificationProposal {
	if proposal == nil || request == nil || request.GetEnvelope() == nil {
		return proposal
	}
	trusted := proto.Clone(proposal).(*reasoningv1.SpecificationProposal)
	envelope := request.GetEnvelope()
	trusted.Identity = &reasoningv1.ProposalIdentity{
		SchemaVersion: envelope.GetSchemaVersion(), RequestId: envelope.GetRequestId(),
		RunId: envelope.GetRunId(), TaskId: envelope.TaskId, Stage: envelope.GetStage(),
		Attempt: envelope.GetAttempt(), AgentManifestDigest: envelope.GetAgentManifestDigest(),
		InputArtifactDigests: artifactDigests(envelope.GetInputArtifacts()),
	}
	return trusted
}

func (s *SpecificationService) replay(
	ctx context.Context, record InvocationRecord,
) (SpecificationOutcome, error) {
	if err := verifyReplayProviderResponse(ctx, s.artifacts, record); err != nil {
		return SpecificationOutcome{}, err
	}
	requestBytes, err := s.artifacts.Get(ctx, record.RequestArtifact)
	if err != nil || verifyArtifact(record.RequestArtifact, requestBytes) != nil {
		return SpecificationOutcome{}, errors.New("load verified specification replay request")
	}
	request := &reasoningv1.SpecificationRequest{}
	if err := proto.Unmarshal(requestBytes, request); err != nil {
		return SpecificationOutcome{}, fmt.Errorf("decode specification replay request: %w", err)
	}
	var proposal *reasoningv1.SpecificationProposal
	if record.ProposalArtifact.URI != "" {
		body, loadErr := s.artifacts.Get(ctx, record.ProposalArtifact)
		if loadErr != nil || verifyArtifact(record.ProposalArtifact, body) != nil {
			return SpecificationOutcome{}, errors.New("load verified specification replay proposal")
		}
		proposal = &reasoningv1.SpecificationProposal{}
		if err := proto.Unmarshal(body, proposal); err != nil {
			return SpecificationOutcome{}, fmt.Errorf("decode specification replay proposal: %w", err)
		}
	}
	if record.Status == StatusAccepted {
		mapped, mapErr := contracts.MapSpecificationRequest(request)
		if mapErr != nil {
			return SpecificationOutcome{}, mapErr
		}
		if _, mapErr = contracts.MapSpecificationProposal(proposal, mapped); mapErr != nil {
			return SpecificationOutcome{}, mapErr
		}
	}
	return specificationOutcome(record, proposal, true), nil
}

func putVerifiedArtifact(ctx context.Context, store ArtifactStore, body []byte) (ArtifactReference, error) {
	reference, err := store.Put(ctx, append([]byte(nil), body...))
	if err != nil {
		return ArtifactReference{}, err
	}
	if err := verifyArtifact(reference, body); err != nil {
		return ArtifactReference{}, err
	}
	return reference, nil
}

func specificationRejection(
	request *reasoningv1.SpecificationRequest, field, message string,
	malformed *MalformedOutput, now time.Time,
) *reasoningv1.ProposalRejection {
	value := &reasoningv1.ProposalRejection{Code: reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
		Summary: message, Retryable: false, Timestamp: timestamppb.New(now),
		Details: []*reasoningv1.RejectionDetail{{Field: field, Message: message}}}
	if malformed != nil {
		failure := &contracts.ValidationFailure{Field: field, Message: message, Kind: malformed.Kind,
			JSONOffset: malformed.JSONOffset, UnknownFields: malformed.UnknownFields,
			ContentBlockTypes: malformed.ContentBlockTypes}
		value.Details = validationFailureDetails(failure)
	}
	if request != nil && request.GetEnvelope() != nil {
		envelope := request.GetEnvelope()
		value.RequestId, value.RunId, value.TaskId = envelope.GetRequestId(), envelope.GetRunId(), envelope.TaskId
		value.Attempt = envelope.GetAttempt()
	}
	return value
}

func specificationOutcome(
	record InvocationRecord, proposal *reasoningv1.SpecificationProposal, replay bool,
) SpecificationOutcome {
	value := SpecificationOutcome{RequestArtifact: record.RequestArtifact,
		ProposalArtifact:         record.ProposalArtifact,
		ProviderResponseArtifact: record.ProviderResponseArtifact, Rejection: record.Rejection,
		Invocation: InvocationMetadata{ProviderRequestID: record.ProviderRequestID,
			Provider: record.Provider, Model: record.Model, StartedAt: record.StartedAt,
			CompletedAt: record.CompletedAt, Usage: record.Usage}, Replay: replay}
	if record.Status == StatusAccepted && proposal != nil {
		value.Proposal = proto.Clone(proposal).(*reasoningv1.SpecificationProposal)
	}
	if value.Rejection != nil {
		value.Rejection = proto.Clone(value.Rejection).(*reasoningv1.ProposalRejection)
	}
	return value
}

func errorMessage(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	return err.Error()
}
