package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	anthropic "github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func enumProjection(value, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(value, prefix))
}

func validReviewProjection(t *testing.T) string {
	t.Helper()
	return reviewProjectionJSON(t, reviewProposalFixture(t))
}

func reviewProjectionJSON(t *testing.T, proposal *reasoningv1.ReviewProposal) string {
	t.Helper()
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

func TestAnthropicReviewAcceptsDocumentedThinkingBeforeOneTextBlock(t *testing.T) {
	message := anthropicMessage(t, validReviewProjection(t))
	message.Content = append([]anthropic.ContentBlockUnion{{
		Type: "thinking", Thinking: "untrusted review reasoning", Signature: "fixture-signature",
	}}, message.Content...)
	sender := &captureMessageSender{reply: message}
	adapter, agentManifest, request := anthropicReviewFixture(t, sender)
	result, err := adapter.ProposeReview(t.Context(), agentManifest, request)
	if err != nil || result.MalformedOutput != nil || result.Proposal == nil {
		t.Fatalf("result=%+v err=%v", result, err)
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
	if result.MalformedOutput.Kind != "unknown_field" ||
		!reflect.DeepEqual(result.MalformedOutput.UnknownFields, []string{"extra"}) ||
		!strings.Contains(result.MalformedOutput.Message, "unknown_field=extra") {
		t.Fatalf("diagnostic=%+v", result.MalformedOutput)
	}
}

func TestAnthropicReviewPreservesPolicyBoundReworkAndUnexpectedPaths(t *testing.T) {
	value := reviewProjection{
		Recommendation: "rework_required",
		Findings: []reviewFindingProjection{{
			FindingID: "FINDING-BLOCKING", Severity: "high", Category: "correctness",
			Summary:            "Independent evidence establishes a blocking defect.",
			EvidenceReferences: []string{"EVIDENCE-001"},
		}},
		RequiredActions: []requiredActionProjection{{
			ActionID: "ACTION-001", FindingID: "FINDING-BLOCKING",
			Description: "Correct the defect and produce new independent evidence.",
		}},
		UnrequestedChanges: []string{"go/internal/unexpected.go"},
		ResidualRisks: []residualRiskProjection{{
			RiskID: "RISK-001", Description: "The implementation narrative is unverified.",
			Severity: "medium",
		}},
		Assumptions: []string{"Implementation claims were not treated as execution evidence."},
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sender := &captureMessageSender{reply: anthropicMessage(t, string(body))}
	adapter, agentManifest, request := anthropicReviewFixture(t, sender)
	result, err := adapter.ProposeReview(t.Context(), agentManifest, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Proposal.GetRecommendation() !=
		reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_REWORK_REQUIRED ||
		result.Proposal.GetUnrequestedChanges()[0] != "go/internal/unexpected.go" ||
		result.Proposal.GetFindings()[0].GetEvidenceReferences()[0] != "EVIDENCE-001" {
		t.Fatalf("review policy, scope, or evidence projection was lost: %+v", result.Proposal)
	}
	mapped, err := contracts.MapReviewRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contracts.MapReviewProposal(result.Proposal, mapped); err != nil {
		t.Fatalf("policy-derived rework failed unchanged kernel validator: %v", err)
	}

	result.Proposal.Recommendation =
		reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_ADVISORY_ACCEPT
	if _, err := contracts.MapReviewProposal(result.Proposal, mapped); err == nil {
		t.Fatal("advisory accept with policy-blocking finding passed kernel validation")
	}
}

func TestAnthropicReviewRepresentsInsufficientEvidenceWithoutFabricatedFinding(t *testing.T) {
	value := reviewProjection{
		Recommendation: "rework_required",
		ResidualRisks: []residualRiskProjection{{
			RiskID: "RISK-EVIDENCE", Description: "Independent evidence is insufficient.",
			Severity: "high",
		}},
		Assumptions: []string{"No implementation narrative was accepted as proof."},
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sender := &captureMessageSender{reply: anthropicMessage(t, string(body))}
	adapter, agentManifest, request := anthropicReviewFixture(t, sender)
	result, err := adapter.ProposeReview(t.Context(), agentManifest, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Proposal.GetFindings()) != 0 ||
		result.Proposal.GetRecommendation() !=
			reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_REWORK_REQUIRED {
		t.Fatalf("insufficient evidence was converted into a fabricated finding: %+v", result.Proposal)
	}
	mapped, err := contracts.MapReviewRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contracts.MapReviewProposal(result.Proposal, mapped); err != nil {
		t.Fatalf("unsupported evaluation representation failed kernel validator: %v", err)
	}
}

func TestAnthropicReviewLoopbackUsesReviewSchema(t *testing.T) {
	var requestBody []byte
	reply := anthropicMessage(t, validReviewProjection(t))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var err error
		requestBody, err = io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if request.Header.Get("X-Api-Key") != "test-review-credential" {
			t.Error("review credential header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply.RawJSON()))
	}))
	defer server.Close()
	adapter, agentManifest, request := anthropicReviewFixture(t, nil)
	adapter.runtime.sender = &sdkMessageSender{
		baseURL: server.URL, timeout: time.Minute, httpClient: server.Client(),
	}
	result, err := adapter.ProposeReview(t.Context(), agentManifest, request)
	if err != nil || result.Proposal == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var body map[string]any
	if err := json.Unmarshal(requestBody, &body); err != nil {
		t.Fatal(err)
	}
	output := body["output_config"].(map[string]any)
	format := output["format"].(map[string]any)
	schema := format["schema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	if properties["recommendation"] == nil || properties["findings"] == nil {
		t.Fatalf("review schema missing from request: %+v", schema)
	}
	finding := properties["findings"].(map[string]any)["items"].(map[string]any)
	findingProperties := finding["properties"].(map[string]any)
	evidenceItems := findingProperties["evidence_references"].(map[string]any)["items"].(map[string]any)
	identifiers := evidenceItems["enum"].([]any)
	if len(identifiers) != len(request.GetIndependentEvidence()) {
		t.Fatalf("evidence enum=%v request evidence=%v", identifiers, request.GetIndependentEvidence())
	}
	for index, evidence := range request.GetIndependentEvidence() {
		if identifiers[index] != evidence.GetEvidenceId() {
			t.Fatalf("evidence enum[%d]=%v want %q", index, identifiers[index], evidence.GetEvidenceId())
		}
	}
}
