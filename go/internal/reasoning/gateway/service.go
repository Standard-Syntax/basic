// Package gateway validates and records provider-neutral reasoning proposals.
package gateway

import (
	"context"
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
)

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
	Proposal *reasoningv1.ImplementationProposal
	Provider string
	Model    string
	Usage    Usage
}

type InvocationMetadata struct {
	Provider    string
	Model       string
	StartedAt   time.Time
	CompletedAt time.Time
	Usage       Usage
}

type ArtifactReference struct {
	URI    string
	SHA256 string
}

type Outcome struct {
	RequestArtifact  ArtifactReference
	ProposalArtifact ArtifactReference
	Proposal         *reasoningv1.ImplementationProposal
	Rejection        *reasoningv1.ProposalRejection
	Invocation       InvocationMetadata
	Replay           bool
}

type Service struct {
	manifests ManifestResolver
	adapter   ImplementationAdapter
	clock     Clock
}

func NewService(
	manifests ManifestResolver, adapter ImplementationAdapter, clock Clock,
) (*Service, error) {
	if manifests == nil || adapter == nil || clock == nil {
		return nil, errors.New("manifest resolver, implementation adapter, and clock are required")
	}
	return &Service{manifests: manifests, adapter: adapter, clock: clock}, nil
}

func (s *Service) ProposeImplementation(
	ctx context.Context, request *reasoningv1.ImplementationRequest,
) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	started := s.clock.Now().UTC()
	mappedRequest, err := contracts.MapImplementationRequestAt(request, started)
	if err != nil {
		if _, ok := contracts.ValidationCode(err); ok {
			return Outcome{Rejection: proposalRejection(request, err, started)}, nil
		}
		return Outcome{}, fmt.Errorf("validate implementation request: %w", err)
	}
	resolved, err := s.manifests.ResolveManifest(
		ctx, mappedRequest.Envelope.AgentManifestDigest,
	)
	if err != nil {
		return Outcome{}, fmt.Errorf("resolve implementation manifest: %w", err)
	}
	if resolved.Digest != mappedRequest.Envelope.AgentManifestDigest ||
		resolved.Manifest.Stage != implementationStage ||
		resolved.Manifest.Output.Schema != implementationOutput {
		return Outcome{}, errors.New("resolved manifest does not match implementation request")
	}
	result, err := s.adapter.ProposeImplementation(ctx, resolved.Manifest, request)
	if err != nil {
		return Outcome{}, fmt.Errorf("propose implementation: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	completed := s.clock.Now().UTC()
	invocation := InvocationMetadata{
		Provider: result.Provider, Model: result.Model,
		StartedAt: started, CompletedAt: completed, Usage: result.Usage,
	}
	if _, err := contracts.MapImplementationProposal(result.Proposal, mappedRequest); err != nil {
		if _, ok := contracts.ValidationCode(err); ok {
			return Outcome{
				Rejection:  proposalRejection(request, err, completed),
				Invocation: invocation,
			}, nil
		}
		return Outcome{}, fmt.Errorf("validate implementation proposal: %w", err)
	}
	return Outcome{
		Proposal:   proto.Clone(result.Proposal).(*reasoningv1.ImplementationProposal),
		Invocation: invocation,
	}, nil
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
