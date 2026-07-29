package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func enumProjection(value, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(value, prefix))
}

func validReviewProjection(t *testing.T) string {
	t.Helper()
	proposal := reviewProposalFixture(t)
	value := reviewProjection{
		Recommendation: enumProjection(
			proposal.GetRecommendation().String(), "REVIEW_RECOMMENDATION_",
		),
		UnrequestedChanges: proposal.GetUnrequestedChanges(),
		Assumptions:        proposal.GetAssumptions(),
	}
	for _, finding := range proposal.GetFindings() {
		value.Findings = append(value.Findings, reviewFindingProjection{
			FindingID:          finding.GetFindingId(),
			Severity:           enumProjection(finding.GetSeverity().String(), "FINDING_SEVERITY_"),
			Category:           enumProjection(finding.GetCategory().String(), "FINDING_CATEGORY_"),
			Summary:            finding.GetSummary(),
			EvidenceReferences: finding.GetEvidenceReferences(),
		})
	}
	for _, action := range proposal.GetRequiredActions() {
		value.RequiredActions = append(value.RequiredActions, requiredActionProjection{
			ActionID: action.GetActionId(), FindingID: action.GetFindingId(),
			Description: action.GetDescription(),
		})
	}
	for _, risk := range proposal.GetResidualRisks() {
		value.ResidualRisks = append(value.ResidualRisks, residualRiskProjection{
			RiskID: risk.GetRiskId(), Description: risk.GetDescription(),
			Severity: enumProjection(risk.GetSeverity().String(), "FINDING_SEVERITY_"),
		})
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func anthropicReviewFixture(
	t *testing.T, sender MessageSender,
) (*AnthropicReviewAdapter, manifest.Manifest, *reasoningv1.ReviewRequest) {
	t.Helper()
	store := newMemoryArtifactStore()
	agentManifest := reviewManifest(t)
	prompt, err := store.Put(t.Context(), []byte("Return an independent review proposal."))
	if err != nil {
		t.Fatal(err)
	}
	agentManifest.Prompt.ArtifactURI, agentManifest.Prompt.SHA256 = prompt.URI, prompt.SHA256
	request := reviewRequestFixture(t)
	request.Envelope.ExpiresAt = timestamppb.New(time.Now().Add(time.Hour))
	request.Envelope.Budget.MaximumInputTokens = 100_000
	reference, err := store.Put(t.Context(), []byte("independent evidence output"))
	if err != nil {
		t.Fatal(err)
	}
	request.Envelope.InputArtifacts = []*reasoningv1.ArtifactDigest{{
		ArtifactUri: reference.URI, Sha256: reference.SHA256,
	}}
	adapter, err := NewAnthropicReviewAdapter(
		credentialSourceFunc(func(context.Context) (string, error) {
			return "test-review-credential", nil
		}),
		StaticCapabilityModels{agentManifest.Model.CapabilityClass: "claude-review-test"},
		store, withAnthropicMessageSender(sender),
	)
	if err != nil {
		t.Fatal(err)
	}
	return adapter, agentManifest, request
}

func TestAnthropicReviewBuildsClosedProposalAcceptedByKernel(t *testing.T) {
	sender := &captureMessageSender{reply: anthropicMessage(t, validReviewProjection(t))}
	adapter, agentManifest, request := anthropicReviewFixture(t, sender)
	result, err := adapter.ProposeReview(t.Context(), agentManifest, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Proposal == nil || result.MalformedOutput != nil ||
		result.Provider != AnthropicProvider || sender.key != "test-review-credential" ||
		sender.params.OutputConfig.Format.Schema["additionalProperties"] != false {
		t.Fatalf("result=%+v params=%+v", result, sender.params)
	}
	mapped, err := contracts.MapReviewRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contracts.MapReviewProposal(result.Proposal, mapped); err != nil {
		t.Fatalf("Anthropic review proposal failed unchanged kernel validator: %v", err)
	}
	if result.Proposal.GetIdentity().GetRequestId() != request.GetEnvelope().GetRequestId() {
		t.Fatal("trusted review identity was not injected")
	}
}

func TestAnthropicReviewMalformedOutputIsTypedResult(t *testing.T) {
	sender := &captureMessageSender{reply: anthropicMessage(
		t, `{"recommendation":"advisory_accept","extra":true}`,
	)}
	adapter, agentManifest, request := anthropicReviewFixture(t, sender)
	result, err := adapter.ProposeReview(t.Context(), agentManifest, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.MalformedOutput == nil || result.Proposal != nil ||
		len(result.ProviderResponse) == 0 {
		t.Fatalf("result=%+v", result)
	}
}
