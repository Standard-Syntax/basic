package gateway

import (
	"context"
	"errors"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"google.golang.org/protobuf/proto"
)

// FakeReviewAdapter is a deterministic, proposal-only review adapter.
type FakeReviewAdapter struct {
	template *reasoningv1.ReviewProposal
	model    string
	usage    Usage
}

func NewFakeReviewAdapter(
	template *reasoningv1.ReviewProposal, model string, usage Usage,
) (*FakeReviewAdapter, error) {
	if template == nil || model == "" {
		return nil, errors.New("review proposal template and fake model are required")
	}
	if usage.ProviderRequests != 1 {
		return nil, errors.New("fake review adapter requires exactly one provider request")
	}
	return &FakeReviewAdapter{
		template: proto.Clone(template).(*reasoningv1.ReviewProposal),
		model:    model,
		usage:    usage,
	}, nil
}

func (a *FakeReviewAdapter) ProposeReview(
	ctx context.Context, _ manifest.Manifest, request *reasoningv1.ReviewRequest,
) (ReviewAdapterResult, error) {
	if err := ctx.Err(); err != nil {
		return ReviewAdapterResult{}, err
	}
	proposal := proto.Clone(a.template).(*reasoningv1.ReviewProposal)
	envelope := request.GetEnvelope()
	proposal.Identity = &reasoningv1.ProposalIdentity{
		SchemaVersion:        envelope.GetSchemaVersion(),
		RequestId:            envelope.GetRequestId(),
		RunId:                envelope.GetRunId(),
		TaskId:               envelope.TaskId,
		Stage:                envelope.GetStage(),
		Attempt:              envelope.GetAttempt(),
		AgentManifestDigest:  envelope.GetAgentManifestDigest(),
		InputArtifactDigests: artifactDigests(envelope.GetInputArtifacts()),
	}
	return ReviewAdapterResult{
		Proposal: proposal, Provider: FakeProvider, Model: a.model, Usage: a.usage,
	}, nil
}
