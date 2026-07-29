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
	reviewStage  = "review"
	reviewOutput = "review_proposal.v1"
)

type ReviewAdapter interface {
	ProposeReview(
		context.Context, manifest.Manifest, *reasoningv1.ReviewRequest,
	) (ReviewAdapterResult, error)
}

type ReviewAdapterResult struct {
	Proposal *reasoningv1.ReviewProposal
	Provider string
	Model    string
	Usage    Usage
}

type ReviewOutcome struct {
	RequestArtifact  ArtifactReference
	ProposalArtifact ArtifactReference
	Proposal         *reasoningv1.ReviewProposal
	Rejection        *reasoningv1.ProposalRejection
	Invocation       InvocationMetadata
	Replay           bool
}

type ReviewService struct {
	manifests   ManifestResolver
	adapter     ReviewAdapter
	artifacts   ArtifactStore
	invocations InvocationRepository
	clock       Clock
	limits      ByteLimits
}

func NewReviewService(
	manifests ManifestResolver,
	adapter ReviewAdapter,
	artifacts ArtifactStore,
	invocations InvocationRepository,
	clock Clock,
	configuredLimits ...ByteLimits,
) (*ReviewService, error) {
	if manifests == nil || adapter == nil || artifacts == nil ||
		invocations == nil || clock == nil {
		return nil, errors.New(
			"manifest resolver, review adapter, artifact store, invocation repository, and clock are required",
		)
	}
	limits := DefaultByteLimits()
	if len(configuredLimits) > 1 {
		return nil, errors.New("at most one byte-limit configuration is allowed")
	}
	if len(configuredLimits) == 1 {
		limits = configuredLimits[0]
	}
	if limits.Request < 1 || limits.Proposal < 1 {
		return nil, errors.New("positive request and proposal byte limits are required")
	}
	return &ReviewService{
		manifests: manifests, adapter: adapter, artifacts: artifacts,
		invocations: invocations, clock: clock, limits: limits,
	}, nil
}

func (s *ReviewService) ProposeReview(
	ctx context.Context, request *reasoningv1.ReviewRequest,
) (ReviewOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ReviewOutcome{}, err
	}
	started := s.clock.Now().UTC()
	requestBytes, earlyOutcome, err := s.prepareReviewRequest(request, started)
	if err != nil {
		return ReviewOutcome{}, err
	}
	if earlyOutcome != nil {
		return *earlyOutcome, nil
	}
	requestArtifact, err := s.putArtifact(ctx, requestBytes)
	if err != nil {
		return ReviewOutcome{}, fmt.Errorf("store review request: %w", err)
	}
	handle, err := s.beginReviewInvocation(ctx, request, requestArtifact, started)
	if err != nil {
		return ReviewOutcome{}, err
	}
	defer rollbackReview(handle)
	if record, ok := handle.Replay(); ok {
		return s.replayReview(ctx, record)
	}
	mapped, rejection, err := validateRecordedReviewRequest(ctx, handle, request, started)
	if err != nil {
		return ReviewOutcome{}, err
	}
	if rejection != nil {
		return *rejection, nil
	}
	result, err := s.invokeReviewAdapter(ctx, request, mapped)
	if err != nil {
		return ReviewOutcome{}, err
	}
	return s.finalizeReview(ctx, handle, request, mapped, result)
}

func (s *ReviewService) prepareReviewRequest(
	request *reasoningv1.ReviewRequest, started time.Time,
) ([]byte, *ReviewOutcome, error) {
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return nil, nil, fmt.Errorf("serialize review request: %w", err)
	}
	if len(requestBytes) > s.limits.Request {
		failure := &contracts.ValidationFailure{
			Code:  reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			Field: "request", Message: "serialized request exceeds byte limit",
		}
		outcome := ReviewOutcome{Rejection: reviewRejection(request, failure, started)}
		return nil, &outcome, nil
	}
	if request == nil || request.GetEnvelope() == nil ||
		request.GetEnvelope().GetRequestId() == "" || request.GetEnvelope().GetAttempt() == 0 {
		_, validationErr := contracts.MapReviewRequestAt(request, started)
		if _, ok := contracts.ValidationCode(validationErr); ok {
			outcome := ReviewOutcome{
				Rejection: reviewRejection(request, validationErr, started),
			}
			return nil, &outcome, nil
		}
		return nil, nil, fmt.Errorf("validate review request: %w", validationErr)
	}
	return requestBytes, nil, nil
}

func (s *ReviewService) beginReviewInvocation(
	ctx context.Context,
	request *reasoningv1.ReviewRequest,
	requestArtifact ArtifactReference,
	started time.Time,
) (InvocationHandle, error) {
	envelope := request.GetEnvelope()
	handle, err := s.invocations.Begin(ctx, InvocationStart{
		RequestID: envelope.GetRequestId(), RequestArtifact: requestArtifact,
		RunID: envelope.GetRunId(), TaskID: envelope.TaskId, Stage: reviewStage,
		Attempt: envelope.GetAttempt(), AgentManifestDigest: envelope.GetAgentManifestDigest(),
		StartedAt: started,
	})
	if err != nil {
		return nil, fmt.Errorf("begin review invocation: %w", err)
	}
	return handle, nil
}

func rollbackReview(handle InvocationHandle) {
	rollbackContext, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	_ = handle.Rollback(rollbackContext)
}

func validateRecordedReviewRequest(
	ctx context.Context,
	handle InvocationHandle,
	request *reasoningv1.ReviewRequest,
	started time.Time,
) (contracts.ReviewRequest, *ReviewOutcome, error) {
	mapped, err := contracts.MapReviewRequestAt(request, started)
	if err == nil {
		return mapped, nil, nil
	}
	if _, ok := contracts.ValidationCode(err); !ok {
		return contracts.ReviewRequest{}, nil, fmt.Errorf("validate review request: %w", err)
	}
	rejection := reviewRejection(request, err, started)
	record, completeErr := handle.Complete(ctx, InvocationCompletion{
		Provider: "gateway", Model: "pre-adapter", CompletedAt: started,
		Status: StatusRejected, Rejection: rejection,
	})
	if completeErr != nil {
		return contracts.ReviewRequest{}, nil, fmt.Errorf(
			"record review rejection: %w", completeErr,
		)
	}
	outcome := reviewOutcomeFromRecord(record, nil, false)
	return contracts.ReviewRequest{}, &outcome, nil
}

func (s *ReviewService) invokeReviewAdapter(
	ctx context.Context,
	request *reasoningv1.ReviewRequest,
	mapped contracts.ReviewRequest,
) (ReviewAdapterResult, error) {
	resolved, err := s.manifests.ResolveManifest(ctx, mapped.Envelope.AgentManifestDigest)
	if err != nil {
		return ReviewAdapterResult{}, fmt.Errorf("resolve review manifest: %w", err)
	}
	if resolved.Digest != mapped.Envelope.AgentManifestDigest ||
		resolved.Manifest.Stage != reviewStage || resolved.Manifest.Output.Schema != reviewOutput {
		return ReviewAdapterResult{}, errors.New("resolved manifest does not match review request")
	}
	result, err := s.adapter.ProposeReview(
		ctx, resolved.Manifest, proto.Clone(request).(*reasoningv1.ReviewRequest),
	)
	if err != nil {
		return ReviewAdapterResult{}, fmt.Errorf("propose review: %w", err)
	}
	if result.Usage.ProviderRequests != 1 ||
		result.Usage.ProviderRequests > mapped.Envelope.MaximumRequests {
		return ReviewAdapterResult{}, errors.New(
			"fake review adapter violated provider request budget",
		)
	}
	return result, nil
}

func (s *ReviewService) finalizeReview(
	ctx context.Context,
	handle InvocationHandle,
	request *reasoningv1.ReviewRequest,
	mapped contracts.ReviewRequest,
	result ReviewAdapterResult,
) (ReviewOutcome, error) {
	completed := s.clock.Now().UTC()
	proposalBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(result.Proposal)
	if err != nil {
		return ReviewOutcome{}, fmt.Errorf("serialize review proposal: %w", err)
	}
	var validationErr error
	if len(proposalBytes) > s.limits.Proposal {
		validationErr = &contracts.ValidationFailure{
			Code:  reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
			Field: "proposal", Message: "serialized proposal exceeds byte limit",
		}
	}
	var proposalArtifact ArtifactReference
	if validationErr == nil {
		proposalArtifact, err = s.putArtifact(ctx, proposalBytes)
		if err != nil {
			return ReviewOutcome{}, fmt.Errorf("store review proposal: %w", err)
		}
		_, validationErr = contracts.MapReviewProposal(result.Proposal, mapped)
	}
	if validationErr != nil {
		if _, ok := contracts.ValidationCode(validationErr); !ok {
			return ReviewOutcome{}, fmt.Errorf("validate review proposal: %w", validationErr)
		}
		rejection := reviewRejection(request, validationErr, completed)
		record, completeErr := handle.Complete(ctx, InvocationCompletion{
			ProposalArtifact: proposalArtifact, Provider: result.Provider, Model: result.Model,
			CompletedAt: completed, Usage: result.Usage, Status: StatusRejected,
			Rejection: rejection,
		})
		if completeErr != nil {
			return ReviewOutcome{}, fmt.Errorf("record review rejection: %w", completeErr)
		}
		return reviewOutcomeFromRecord(record, nil, false), nil
	}
	record, err := handle.Complete(ctx, InvocationCompletion{
		ProposalArtifact: proposalArtifact, Provider: result.Provider, Model: result.Model,
		CompletedAt: completed, Usage: result.Usage, Status: StatusAccepted,
	})
	if err != nil {
		return ReviewOutcome{}, fmt.Errorf("record review proposal: %w", err)
	}
	return reviewOutcomeFromRecord(record, result.Proposal, false), nil
}

func (s *ReviewService) replayReview(
	ctx context.Context, record InvocationRecord,
) (ReviewOutcome, error) {
	requestBytes, err := s.artifacts.Get(ctx, record.RequestArtifact)
	if err != nil {
		return ReviewOutcome{}, fmt.Errorf("load replay review request: %w", err)
	}
	if err := verifyArtifact(record.RequestArtifact, requestBytes); err != nil {
		return ReviewOutcome{}, err
	}
	var request reasoningv1.ReviewRequest
	if err := proto.Unmarshal(requestBytes, &request); err != nil {
		return ReviewOutcome{}, fmt.Errorf("decode replay review request: %w", err)
	}
	var proposal *reasoningv1.ReviewProposal
	if record.ProposalArtifact.URI != "" {
		proposalBytes, loadErr := s.artifacts.Get(ctx, record.ProposalArtifact)
		if loadErr != nil {
			return ReviewOutcome{}, fmt.Errorf("load replay review proposal: %w", loadErr)
		}
		if err := verifyArtifact(record.ProposalArtifact, proposalBytes); err != nil {
			return ReviewOutcome{}, err
		}
		proposal = &reasoningv1.ReviewProposal{}
		if err := proto.Unmarshal(proposalBytes, proposal); err != nil {
			return ReviewOutcome{}, fmt.Errorf("decode replay review proposal: %w", err)
		}
	}
	if record.Status == StatusAccepted {
		mapped, err := contracts.MapReviewRequest(&request)
		if err != nil {
			return ReviewOutcome{}, fmt.Errorf("validate replay review request: %w", err)
		}
		if _, err := contracts.MapReviewProposal(proposal, mapped); err != nil {
			return ReviewOutcome{}, fmt.Errorf("validate replay review proposal: %w", err)
		}
	}
	return reviewOutcomeFromRecord(record, proposal, true), nil
}

func (s *ReviewService) putArtifact(
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

func reviewOutcomeFromRecord(
	record InvocationRecord, proposal *reasoningv1.ReviewProposal, replay bool,
) ReviewOutcome {
	outcome := ReviewOutcome{
		RequestArtifact: record.RequestArtifact, ProposalArtifact: record.ProposalArtifact,
		Rejection: record.Rejection, Replay: replay,
		Invocation: InvocationMetadata{
			Provider: record.Provider, Model: record.Model, StartedAt: record.StartedAt,
			CompletedAt: record.CompletedAt, Usage: record.Usage,
		},
	}
	if record.Status == StatusAccepted && proposal != nil {
		outcome.Proposal = proto.Clone(proposal).(*reasoningv1.ReviewProposal)
	}
	if outcome.Rejection != nil {
		outcome.Rejection = proto.Clone(outcome.Rejection).(*reasoningv1.ProposalRejection)
	}
	return outcome
}

func reviewRejection(
	request *reasoningv1.ReviewRequest, err error, now time.Time,
) *reasoningv1.ProposalRejection {
	code, _ := contracts.ValidationCode(err)
	rejection := &reasoningv1.ProposalRejection{
		Code: code, Summary: err.Error(), Retryable: false, Timestamp: timestamppb.New(now),
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
