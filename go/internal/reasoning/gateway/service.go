// Package gateway validates and records provider-neutral reasoning proposals.
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"github.com/Standard-Syntax/basic/go/internal/registry"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	implementationStage  = "implementation"
	implementationOutput = "implementation_proposal.v1"
	rollbackTimeout      = 5 * time.Second
	defaultMaximumBytes  = 1 << 20
)

type ByteLimits struct {
	Request          int
	Proposal         int
	ProviderResponse int
}

func DefaultByteLimits() ByteLimits {
	return ByteLimits{
		Request: defaultMaximumBytes, Proposal: defaultMaximumBytes,
		ProviderResponse: defaultMaximumBytes,
	}
}

type Clock interface {
	Now() time.Time
}

type ManifestResolver interface {
	ResolveManifest(context.Context, string) (ResolvedManifest, error)
}

type ImplementationAdapter interface {
	ProposeImplementation(
		context.Context, manifest.Manifest, *reasoningv1.ImplementationRequest,
	) (AdapterResult, error)
}

type ResolvedManifest struct {
	Digest   string
	Manifest manifest.Manifest
}

type Usage struct {
	InputTokens      uint64
	OutputTokens     uint64
	ProviderRequests uint32
}

type AdapterResult struct {
	Proposal          *reasoningv1.ImplementationProposal
	ProviderResponse  []byte
	ProviderRequestID string
	MalformedOutput   *MalformedOutput
	Provider          string
	Model             string
	Usage             Usage
}

// MalformedOutput is a deterministic provider response that could not be
// projected into the closed stage schema. Adapters return transport and
// provider failures as errors instead.
type MalformedOutput struct {
	Message string
}

type InvocationMetadata struct {
	ProviderRequestID string
	Provider          string
	Model             string
	StartedAt         time.Time
	CompletedAt       time.Time
	Usage             Usage
}

type ArtifactReference struct {
	URI    string
	SHA256 string
}

type Outcome struct {
	RequestArtifact          ArtifactReference
	ProposalArtifact         ArtifactReference
	ProviderResponseArtifact ArtifactReference
	Proposal                 *reasoningv1.ImplementationProposal
	Rejection                *reasoningv1.ProposalRejection
	Invocation               InvocationMetadata
	Replay                   bool
}

type Service struct {
	manifests   ManifestResolver
	adapter     ImplementationAdapter
	artifacts   ArtifactStore
	invocations InvocationRepository
	clock       Clock
	limits      ByteLimits
}

func NewService(
	manifests ManifestResolver,
	adapter ImplementationAdapter,
	artifacts ArtifactStore,
	invocations InvocationRepository,
	clock Clock,
	configuredLimits ...ByteLimits,
) (*Service, error) {
	if manifests == nil || adapter == nil || artifacts == nil ||
		invocations == nil || clock == nil {
		return nil, errors.New(
			"manifest resolver, adapter, artifact store, invocation repository, and clock are required",
		)
	}
	limits := DefaultByteLimits()
	if len(configuredLimits) > 1 {
		return nil, errors.New("at most one byte-limit configuration is allowed")
	}
	if len(configuredLimits) == 1 {
		limits = configuredLimits[0]
	}
	if limits.Request < 1 || limits.Proposal < 1 || limits.ProviderResponse < 1 {
		return nil, errors.New(
			"positive request, proposal, and provider-response byte limits are required",
		)
	}
	return &Service{
		manifests: manifests, adapter: adapter, artifacts: artifacts,
		invocations: invocations, clock: clock, limits: limits,
	}, nil
}

func (s *Service) ProposeImplementation(
	ctx context.Context, request *reasoningv1.ImplementationRequest,
) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	started := s.clock.Now().UTC()
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return Outcome{}, fmt.Errorf("serialize implementation request: %w", err)
	}
	if len(requestBytes) > s.limits.Request {
		failure := &contracts.ValidationFailure{
			Code:  reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			Field: "request", Message: "serialized request exceeds byte limit",
		}
		return Outcome{Rejection: proposalRejection(request, failure, started)}, nil
	}
	if outcome, err := preflightRequest(request, started); outcome != nil || err != nil {
		return valueOrEmptyOutcome(outcome), err
	}
	requestArtifact, err := s.putArtifact(ctx, requestBytes)
	if err != nil {
		return Outcome{}, fmt.Errorf("store implementation request: %w", err)
	}
	envelope := request.GetEnvelope()
	handle, err := s.invocations.Begin(ctx, InvocationStart{
		RequestID: envelope.GetRequestId(), RequestArtifact: requestArtifact,
		RunID: envelope.GetRunId(), TaskID: envelope.TaskId,
		Stage: implementationStage, Attempt: envelope.GetAttempt(),
		AgentManifestDigest: envelope.GetAgentManifestDigest(), StartedAt: started,
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("begin reasoning invocation: %w", err)
	}
	defer func() {
		rollbackContext, cancel := context.WithTimeout(
			context.Background(), rollbackTimeout,
		)
		defer cancel()
		_ = handle.Rollback(rollbackContext)
	}()
	if record, ok := handle.Replay(); ok {
		return s.replayOutcome(ctx, record)
	}

	mappedRequest, outcome, err := validateRecordedRequest(
		ctx, handle, request, started,
	)
	if err != nil {
		return Outcome{}, err
	}
	if outcome != nil {
		return *outcome, nil
	}
	result, err := s.invokeImplementation(ctx, request, mappedRequest)
	if err != nil {
		return Outcome{}, err
	}
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	if err := validateProviderBudget(result, mappedRequest); err != nil {
		return Outcome{}, err
	}
	return s.finalizeProposal(ctx, handle, request, mappedRequest, result)
}

func preflightRequest(
	request *reasoningv1.ImplementationRequest, started time.Time,
) (*Outcome, error) {
	if request != nil && request.GetEnvelope() != nil &&
		request.GetEnvelope().GetRequestId() != "" &&
		request.GetEnvelope().GetAttempt() != 0 {
		return nil, nil
	}
	_, err := contracts.MapImplementationRequestAt(request, started)
	if _, ok := contracts.ValidationCode(err); ok {
		return &Outcome{Rejection: proposalRejection(request, err, started)}, nil
	}
	return nil, fmt.Errorf("validate implementation request: %w", err)
}

func validateRecordedRequest(
	ctx context.Context,
	handle InvocationHandle,
	request *reasoningv1.ImplementationRequest,
	started time.Time,
) (contracts.ImplementationRequest, *Outcome, error) {
	mapped, err := contracts.MapImplementationRequestAt(request, started)
	if err == nil {
		return mapped, nil, nil
	}
	if _, ok := contracts.ValidationCode(err); !ok {
		return contracts.ImplementationRequest{}, nil, fmt.Errorf(
			"validate implementation request: %w", err,
		)
	}
	rejection := proposalRejection(request, err, started)
	record, completeErr := handle.Complete(ctx, InvocationCompletion{
		Provider: "gateway", Model: "pre-adapter", CompletedAt: started,
		Status: StatusRejected, Rejection: rejection,
	})
	if completeErr != nil {
		return contracts.ImplementationRequest{}, nil, fmt.Errorf(
			"record implementation rejection: %w", completeErr,
		)
	}
	outcome := outcomeFromRecord(record, nil, false)
	return contracts.ImplementationRequest{}, &outcome, nil
}

func (s *Service) invokeImplementation(
	ctx context.Context,
	request *reasoningv1.ImplementationRequest,
	mapped contracts.ImplementationRequest,
) (AdapterResult, error) {
	resolved, err := s.manifests.ResolveManifest(
		ctx, mapped.Envelope.AgentManifestDigest,
	)
	if err != nil {
		return AdapterResult{}, fmt.Errorf("resolve implementation manifest: %w", err)
	}
	if resolved.Digest != mapped.Envelope.AgentManifestDigest ||
		resolved.Manifest.Stage != implementationStage ||
		resolved.Manifest.Output.Schema != implementationOutput {
		return AdapterResult{}, errors.New(
			"resolved manifest does not match implementation request",
		)
	}
	adapterRequest := proto.Clone(request).(*reasoningv1.ImplementationRequest)
	result, err := s.adapter.ProposeImplementation(ctx, resolved.Manifest, adapterRequest)
	if err != nil {
		return AdapterResult{}, fmt.Errorf("propose implementation: %w", err)
	}
	return result, nil
}

func validateProviderBudget(
	result AdapterResult, request contracts.ImplementationRequest,
) error {
	if result.Usage.ProviderRequests < 1 ||
		result.Usage.ProviderRequests > request.Envelope.MaximumRequests {
		return errors.New("implementation adapter violated provider request budget")
	}
	return nil
}

func (s *Service) finalizeProposal(
	ctx context.Context,
	handle InvocationHandle,
	request *reasoningv1.ImplementationRequest,
	mapped contracts.ImplementationRequest,
	result AdapterResult,
) (Outcome, error) {
	completed := s.clock.Now().UTC()
	if len(result.ProviderResponse) > s.limits.ProviderResponse {
		return Outcome{}, errors.New("provider response exceeds byte limit")
	}
	responseArtifact, err := s.putArtifact(ctx, result.ProviderResponse)
	if err != nil {
		return Outcome{}, fmt.Errorf("store provider response: %w", err)
	}
	completion := InvocationCompletion{
		ProviderResponseArtifact: responseArtifact,
		ProviderRequestID:        result.ProviderRequestID,
		Provider:                 result.Provider, Model: result.Model,
		CompletedAt: completed, Usage: result.Usage,
	}
	if result.MalformedOutput != nil {
		message := result.MalformedOutput.Message
		if message == "" {
			message = "provider output does not match the closed implementation schema"
		}
		failure := &contracts.ValidationFailure{
			Code:  reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			Field: "provider_response", Message: message,
		}
		completion.Status = StatusRejected
		completion.Rejection = proposalRejection(request, failure, completed)
		record, completeErr := handle.Complete(ctx, completion)
		if completeErr != nil {
			return Outcome{}, fmt.Errorf("record malformed implementation output: %w", completeErr)
		}
		return outcomeFromRecord(record, nil, false), nil
	}
	proposalBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(result.Proposal)
	if err != nil {
		return Outcome{}, fmt.Errorf("serialize implementation proposal: %w", err)
	}
	if len(proposalBytes) > s.limits.Proposal {
		failure := &contracts.ValidationFailure{
			Code:  reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			Field: "proposal", Message: "serialized proposal exceeds byte limit",
		}
		rejection := proposalRejection(request, failure, completed)
		completion.Status, completion.Rejection = StatusRejected, rejection
		record, completeErr := handle.Complete(ctx, completion)
		if completeErr != nil {
			return Outcome{}, fmt.Errorf(
				"record oversized implementation proposal: %w", completeErr,
			)
		}
		return outcomeFromRecord(record, nil, false), nil
	}
	proposalArtifact, err := s.putArtifact(ctx, proposalBytes)
	if err != nil {
		return Outcome{}, fmt.Errorf("store implementation proposal: %w", err)
	}
	if _, err := contracts.MapImplementationProposal(result.Proposal, mapped); err != nil {
		if _, ok := contracts.ValidationCode(err); ok {
			rejection := proposalRejection(request, err, completed)
			completion.ProposalArtifact = proposalArtifact
			completion.Status, completion.Rejection = StatusRejected, rejection
			record, completeErr := handle.Complete(ctx, completion)
			if completeErr != nil {
				return Outcome{}, fmt.Errorf(
					"record implementation rejection: %w", completeErr,
				)
			}
			return outcomeFromRecord(record, nil, false), nil
		}
		return Outcome{}, fmt.Errorf("validate implementation proposal: %w", err)
	}
	completion.ProposalArtifact = proposalArtifact
	completion.Status = StatusAccepted
	record, err := handle.Complete(ctx, completion)
	if err != nil {
		return Outcome{}, fmt.Errorf("record implementation proposal: %w", err)
	}
	return outcomeFromRecord(record, result.Proposal, false), nil
}

func valueOrEmptyOutcome(outcome *Outcome) Outcome {
	if outcome == nil {
		return Outcome{}
	}
	return *outcome
}

func (s *Service) putArtifact(
	ctx context.Context, body []byte,
) (ArtifactReference, error) {
	reference, err := s.artifacts.Put(ctx, append([]byte(nil), body...))
	if err != nil {
		return ArtifactReference{}, err
	}
	if err := verifyArtifact(reference, body); err != nil {
		return ArtifactReference{}, err
	}
	return reference, nil
}

func (s *Service) replayOutcome(
	ctx context.Context, record InvocationRecord,
) (Outcome, error) {
	if err := verifyReplayProviderResponse(ctx, s.artifacts, record); err != nil {
		return Outcome{}, err
	}
	requestBytes, err := s.artifacts.Get(ctx, record.RequestArtifact)
	if err != nil {
		return Outcome{}, fmt.Errorf("load replay request artifact: %w", err)
	}
	if err := verifyArtifact(record.RequestArtifact, requestBytes); err != nil {
		return Outcome{}, err
	}
	var request reasoningv1.ImplementationRequest
	if err := proto.Unmarshal(requestBytes, &request); err != nil {
		return Outcome{}, fmt.Errorf("decode replay request artifact: %w", err)
	}
	var proposal *reasoningv1.ImplementationProposal
	if record.ProposalArtifact.URI != "" {
		proposalBytes, err := s.artifacts.Get(ctx, record.ProposalArtifact)
		if err != nil {
			return Outcome{}, fmt.Errorf("load replay proposal artifact: %w", err)
		}
		if err := verifyArtifact(record.ProposalArtifact, proposalBytes); err != nil {
			return Outcome{}, err
		}
		proposal = &reasoningv1.ImplementationProposal{}
		if err := proto.Unmarshal(proposalBytes, proposal); err != nil {
			return Outcome{}, fmt.Errorf("decode replay proposal artifact: %w", err)
		}
	}
	if record.Status == StatusAccepted {
		mapped, err := contracts.MapImplementationRequest(&request)
		if err != nil {
			return Outcome{}, fmt.Errorf("validate replay request: %w", err)
		}
		if _, err := contracts.MapImplementationProposal(proposal, mapped); err != nil {
			return Outcome{}, fmt.Errorf("validate replay proposal: %w", err)
		}
	}
	return outcomeFromRecord(record, proposal, true), nil
}

func verifyReplayProviderResponse(
	ctx context.Context, artifacts ArtifactStore, record InvocationRecord,
) error {
	if record.ProviderResponseArtifact.URI == "" {
		return nil
	}
	body, err := artifacts.Get(ctx, record.ProviderResponseArtifact)
	if err != nil {
		return fmt.Errorf("load replay provider response artifact: %w", err)
	}
	if err := verifyArtifact(record.ProviderResponseArtifact, body); err != nil {
		return err
	}
	return nil
}

func outcomeFromRecord(
	record InvocationRecord,
	proposal *reasoningv1.ImplementationProposal,
	replay bool,
) Outcome {
	outcome := Outcome{
		RequestArtifact: record.RequestArtifact, ProposalArtifact: record.ProposalArtifact,
		ProviderResponseArtifact: record.ProviderResponseArtifact,
		Rejection:                record.Rejection,
		Invocation: InvocationMetadata{
			ProviderRequestID: record.ProviderRequestID,
			Provider:          record.Provider, Model: record.Model,
			StartedAt: record.StartedAt, CompletedAt: record.CompletedAt,
			Usage: record.Usage,
		},
		Replay: replay,
	}
	if record.Status == StatusAccepted && proposal != nil {
		outcome.Proposal = proto.Clone(proposal).(*reasoningv1.ImplementationProposal)
	}
	if outcome.Rejection != nil {
		outcome.Rejection = proto.Clone(outcome.Rejection).(*reasoningv1.ProposalRejection)
	}
	return outcome
}

func verifyArtifact(reference ArtifactReference, body []byte) error {
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if reference.SHA256 != digest ||
		reference.URI != "artifact://sha256/"+digest {
		return ErrArtifactIntegrity
	}
	return nil
}

func proposalRejection(
	request *reasoningv1.ImplementationRequest, err error, now time.Time,
) *reasoningv1.ProposalRejection {
	code, _ := contracts.ValidationCode(err)
	rejection := &reasoningv1.ProposalRejection{
		Code: code, Summary: err.Error(), Retryable: false,
		Timestamp: timestamppb.New(now),
	}
	if request == nil || request.GetEnvelope() == nil {
		return rejection
	}
	envelope := request.GetEnvelope()
	rejection.RequestId = envelope.GetRequestId()
	rejection.RunId = envelope.GetRunId()
	rejection.TaskId = envelope.TaskId
	rejection.Attempt = envelope.GetAttempt()
	var failure *contracts.ValidationFailure
	if errors.As(err, &failure) {
		rejection.Details = []*reasoningv1.RejectionDetail{{
			Field: failure.Field, Message: failure.Message,
		}}
	}
	return rejection
}

type systemClock struct{}

func (systemClock) Now() time.Time {
	return time.Now()
}

func NewSystemClock() Clock {
	return systemClock{}
}

type registryLookup interface {
	GetByDigest(context.Context, string) (registry.Record, error)
}

type RegistryManifestResolver struct {
	registry registryLookup
}

func NewRegistryManifestResolver(value registryLookup) (*RegistryManifestResolver, error) {
	if value == nil {
		return nil, errors.New("registry is required")
	}
	return &RegistryManifestResolver{registry: value}, nil
}

func (r *RegistryManifestResolver) ResolveManifest(
	ctx context.Context, digest string,
) (ResolvedManifest, error) {
	record, err := r.registry.GetByDigest(ctx, digest)
	if err != nil {
		return ResolvedManifest{}, err
	}
	return ResolvedManifest{Digest: record.Digest, Manifest: record.Manifest}, nil
}
