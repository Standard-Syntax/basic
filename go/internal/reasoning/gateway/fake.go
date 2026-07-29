package gateway

import (
	"context"
	"errors"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"google.golang.org/protobuf/proto"
)

const FakeProvider = "deterministic-fake"

type FakeImplementationAdapter struct {
	template *reasoningv1.ImplementationProposal
	model    string
	usage    Usage
}

func NewFakeImplementationAdapter(
	template *reasoningv1.ImplementationProposal, model string, usage Usage,
) (*FakeImplementationAdapter, error) {
	if template == nil || model == "" {
		return nil, errors.New("proposal template and fake model are required")
	}
	if usage.ProviderRequests != 1 {
		return nil, errors.New("fake adapter requires exactly one provider request")
	}
	return &FakeImplementationAdapter{
		template: proto.Clone(template).(*reasoningv1.ImplementationProposal),
		model:    model, usage: usage,
	}, nil
}

func (a *FakeImplementationAdapter) ProposeImplementation(
	ctx context.Context, _ manifest.Manifest, request *reasoningv1.ImplementationRequest,
) (AdapterResult, error) {
	if err := ctx.Err(); err != nil {
		return AdapterResult{}, err
	}
	proposal := proto.Clone(a.template).(*reasoningv1.ImplementationProposal)
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
	proposal.ApprovedTaskId = request.GetApprovedTaskId()
	proposal.ApprovedTaskDigest = request.GetApprovedTaskDigest()
	proposal.ApprovedSpecificationDigest = request.GetApprovedSpecificationDigest()
	response, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposal)
	if err != nil {
		return AdapterResult{}, err
	}
	return AdapterResult{
		Proposal: proposal, ProviderResponse: response,
		Provider: FakeProvider, Model: a.model, Usage: a.usage,
	}, nil
}

func artifactDigests(values []*reasoningv1.ArtifactDigest) []string {
	digests := make([]string, len(values))
	for index, value := range values {
		digests[index] = value.GetSha256()
	}
	return digests
}
