package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"google.golang.org/protobuf/encoding/protojson"
)

type AnthropicReviewAdapter struct {
	runtime *anthropicRuntime
}

func NewAnthropicReviewAdapter(
	credentials CredentialSource,
	models CapabilityModelResolver,
	artifacts ArtifactStore,
	options ...AnthropicOption,
) (*AnthropicReviewAdapter, error) {
	runtime, err := newAnthropicRuntime(credentials, models, artifacts, options...)
	if err != nil {
		return nil, err
	}
	return &AnthropicReviewAdapter{runtime: runtime}, nil
}

func (a *AnthropicReviewAdapter) ProposeReview(
	ctx context.Context,
	agentManifest manifest.Manifest,
	request *reasoningv1.ReviewRequest,
) (ReviewAdapterResult, error) {
	if request == nil || request.GetEnvelope() == nil {
		return ReviewAdapterResult{}, errors.New("review request envelope is required")
	}
	key, model, err := a.runtime.invocationConfiguration(
		ctx, agentManifest.Model.CapabilityClass,
	)
	if err != nil {
		return ReviewAdapterResult{}, err
	}
	defer clearString(&key)
	system, user, err := a.runtime.renderReview(ctx, key, agentManifest, request)
	if err != nil {
		return ReviewAdapterResult{}, err
	}
	params := anthropic.MessageNewParams{
		MaxTokens: int64(minPositive(
			uint64(agentManifest.Model.MaximumOutputTokens),
			request.GetEnvelope().GetBudget().GetMaximumOutputTokens(),
		)),
		Model:  anthropic.Model(model),
		System: []anthropic.TextBlockParam{{Text: system}},
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(
			anthropic.NewTextBlock(user),
		)},
		Temperature: param.NewOpt(agentManifest.Model.Temperature),
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{Schema: reviewOutputSchema()},
		},
	}
	message, attempts, err := a.runtime.sendWithRetry(
		ctx, key, &params,
		request.GetEnvelope().GetBudget().GetMaximumProviderRequests(),
		request.GetEnvelope().GetExpiresAt().AsTime(),
	)
	if err != nil {
		return ReviewAdapterResult{}, err
	}
	if message.StopReason == anthropic.StopReasonRefusal {
		return ReviewAdapterResult{}, &ProviderError{
			Kind: ProviderErrorRefusal, RequestID: message.ID, Attempts: attempts,
		}
	}
	result := ReviewAdapterResult{
		ProviderResponse: []byte(message.RawJSON()), ProviderRequestID: message.ID,
		Provider: AnthropicProvider, Model: string(message.Model),
		Usage: Usage{
			InputTokens: uint64(maxInt64(
				0, message.Usage.InputTokens+
					message.Usage.CacheCreationInputTokens+
					message.Usage.CacheReadInputTokens,
			)),
			OutputTokens:     uint64(maxInt64(0, message.Usage.OutputTokens)),
			ProviderRequests: attempts,
		},
	}
	budget := request.GetEnvelope().GetBudget()
	if result.Usage.InputTokens > budget.GetMaximumInputTokens() ||
		result.Usage.OutputTokens > budget.GetMaximumOutputTokens() {
		result.MalformedOutput = &MalformedOutput{
			Message: "provider token usage exceeds the trusted request budget",
		}
		return result, nil
	}
	projection, malformed := decodeReviewMessage(message)
	if malformed != nil {
		result.MalformedOutput = malformed
		return result, nil
	}
	result.Proposal = reviewProposalFromProjection(projection, request)
	return result, nil
}

func (r *anthropicRuntime) renderReview(
	ctx context.Context,
	key string,
	agentManifest manifest.Manifest,
	request *reasoningv1.ReviewRequest,
) (string, string, error) {
	prompt, err := r.readVerifiedArtifact(ctx, ArtifactReference{
		URI: agentManifest.Prompt.ArtifactURI, SHA256: agentManifest.Prompt.SHA256,
	})
	if err != nil {
		return "", "", fmt.Errorf("load verified manifest prompt: %w", err)
	}
	if err := guardProviderContent(key, prompt); err != nil {
		return "", "", err
	}
	requestJSON, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(request)
	if err != nil {
		return "", "", fmt.Errorf("render review request: %w", err)
	}
	contextValue := struct {
		Request        json.RawMessage    `json:"request"`
		InputArtifacts []renderedArtifact `json:"input_artifacts"`
	}{Request: requestJSON}
	for _, artifact := range request.GetEnvelope().GetInputArtifacts() {
		body, readErr := r.readVerifiedArtifact(ctx, ArtifactReference{
			URI: artifact.GetArtifactUri(), SHA256: artifact.GetSha256(),
		})
		if readErr != nil {
			return "", "", fmt.Errorf("load verified input artifact: %w", readErr)
		}
		if err := guardProviderContent(key, body); err != nil {
			return "", "", err
		}
		contextValue.InputArtifacts = append(contextValue.InputArtifacts, renderedArtifact{
			URI: artifact.GetArtifactUri(), SHA256: artifact.GetSha256(), Content: string(body),
		})
	}
	user, err := json.Marshal(contextValue)
	if err != nil {
		return "", "", fmt.Errorf("render review context: %w", err)
	}
	maximumTokens := minPositive(
		uint64(agentManifest.Context.MaximumContextTokens),
		request.GetEnvelope().GetBudget().GetMaximumInputTokens(),
	)
	if maximumTokens == 0 ||
		uint64((len(prompt)+len(user)+estimatedBytesPerToken-1)/estimatedBytesPerToken) >
			maximumTokens {
		return "", "", errors.New("rendered provider context exceeds input token budget")
	}
	return string(prompt), string(user), nil
}

type reviewProjection struct {
	Recommendation     string                     `json:"recommendation"`
	Findings           []reviewFindingProjection  `json:"findings"`
	RequiredActions    []requiredActionProjection `json:"required_actions"`
	UnrequestedChanges []string                   `json:"unrequested_changes"`
	ResidualRisks      []residualRiskProjection   `json:"residual_risks"`
	Assumptions        []string                   `json:"assumptions"`
}

type reviewFindingProjection struct {
	FindingID          string   `json:"finding_id"`
	Severity           string   `json:"severity"`
	Category           string   `json:"category"`
	Summary            string   `json:"summary"`
	EvidenceReferences []string `json:"evidence_references"`
}

type requiredActionProjection struct {
	ActionID    string `json:"action_id"`
	FindingID   string `json:"finding_id"`
	Description string `json:"description"`
}

type residualRiskProjection struct {
	RiskID      string `json:"risk_id"`
	Description string `json:"description"`
	Severity    string `json:"severity"`
}

func decodeReviewMessage(
	message *anthropic.Message,
) (reviewProjection, *MalformedOutput) {
	if message == nil || message.StopReason != anthropic.StopReasonEndTurn ||
		len(message.Content) != 1 || message.Content[0].Type != "text" ||
		strings.TrimSpace(message.Content[0].Text) == "" {
		return reviewProjection{}, &MalformedOutput{
			Message: "provider response must contain one complete text block",
		}
	}
	var projection reviewProjection
	decoder := json.NewDecoder(strings.NewReader(message.Content[0].Text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&projection); err != nil {
		return reviewProjection{}, &MalformedOutput{
			Message: "provider response is not valid review JSON",
		}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return reviewProjection{}, &MalformedOutput{
			Message: "provider response contains trailing JSON",
		}
	}
	return projection, nil
}

func reviewProposalFromProjection(
	value reviewProjection, request *reasoningv1.ReviewRequest,
) *reasoningv1.ReviewProposal {
	envelope := request.GetEnvelope()
	proposal := &reasoningv1.ReviewProposal{
		Identity: &reasoningv1.ProposalIdentity{
			SchemaVersion: envelope.GetSchemaVersion(), RequestId: envelope.GetRequestId(),
			RunId: envelope.GetRunId(), TaskId: envelope.TaskId, Stage: envelope.GetStage(),
			Attempt: envelope.GetAttempt(), AgentManifestDigest: envelope.GetAgentManifestDigest(),
			InputArtifactDigests: artifactDigests(envelope.GetInputArtifacts()),
		},
		Recommendation:     reviewRecommendationFromString(value.Recommendation),
		UnrequestedChanges: value.UnrequestedChanges, Assumptions: value.Assumptions,
	}
	for _, finding := range value.Findings {
		proposal.Findings = append(proposal.Findings, &reasoningv1.ReviewFinding{
			FindingId: finding.FindingID,
			Severity:  findingSeverityFromString(finding.Severity),
			Category:  findingCategoryFromString(finding.Category),
			Summary:   finding.Summary, EvidenceReferences: finding.EvidenceReferences,
		})
	}
	for _, action := range value.RequiredActions {
		proposal.RequiredActions = append(proposal.RequiredActions, &reasoningv1.RequiredAction{
			ActionId: action.ActionID, FindingId: action.FindingID,
			Description: action.Description,
		})
	}
	for _, risk := range value.ResidualRisks {
		proposal.ResidualRisks = append(proposal.ResidualRisks, &reasoningv1.ResidualRisk{
			RiskId: risk.RiskID, Description: risk.Description,
			Severity: findingSeverityFromString(risk.Severity),
		})
	}
	return proposal
}

func reviewRecommendationFromString(value string) reasoningv1.ReviewRecommendation {
	switch value {
	case "rework_required":
		return reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_REWORK_REQUIRED
	case "advisory_accept":
		return reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_ADVISORY_ACCEPT
	default:
		return reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_UNSPECIFIED
	}
}

func findingSeverityFromString(value string) reasoningv1.FindingSeverity {
	values := map[string]reasoningv1.FindingSeverity{
		"info":     reasoningv1.FindingSeverity_FINDING_SEVERITY_INFO,
		"low":      reasoningv1.FindingSeverity_FINDING_SEVERITY_LOW,
		"medium":   reasoningv1.FindingSeverity_FINDING_SEVERITY_MEDIUM,
		"high":     reasoningv1.FindingSeverity_FINDING_SEVERITY_HIGH,
		"critical": reasoningv1.FindingSeverity_FINDING_SEVERITY_CRITICAL,
	}
	return values[value]
}

func findingCategoryFromString(value string) reasoningv1.FindingCategory {
	values := map[string]reasoningv1.FindingCategory{
		"correctness":     reasoningv1.FindingCategory_FINDING_CATEGORY_CORRECTNESS,
		"security":        reasoningv1.FindingCategory_FINDING_CATEGORY_SECURITY,
		"scope":           reasoningv1.FindingCategory_FINDING_CATEGORY_SCOPE,
		"testing":         reasoningv1.FindingCategory_FINDING_CATEGORY_TESTING,
		"maintainability": reasoningv1.FindingCategory_FINDING_CATEGORY_MAINTAINABILITY,
		"compatibility":   reasoningv1.FindingCategory_FINDING_CATEGORY_COMPATIBILITY,
	}
	return values[value]
}

func reviewOutputSchema() map[string]any {
	stringArray := map[string]any{"type": "array", "items": stringSchema()}
	severity := map[string]any{
		"type": "string",
		"enum": []string{"info", "low", "medium", "high", "critical"},
	}
	finding := closedObject(map[string]any{
		"finding_id": stringSchema(), "severity": severity,
		"category": map[string]any{
			"type": "string",
			"enum": []string{
				"correctness", "security", "scope", "testing",
				"maintainability", "compatibility",
			},
		},
		"summary": stringSchema(), "evidence_references": stringArray,
	}, []string{"finding_id", "severity", "category", "summary", "evidence_references"})
	action := closedObject(map[string]any{
		"action_id": stringSchema(), "finding_id": stringSchema(),
		"description": stringSchema(),
	}, []string{"action_id", "finding_id", "description"})
	risk := closedObject(map[string]any{
		"risk_id": stringSchema(), "description": stringSchema(), "severity": severity,
	}, []string{"risk_id", "description", "severity"})
	return closedObject(map[string]any{
		"recommendation": map[string]any{
			"type": "string", "enum": []string{"rework_required", "advisory_accept"},
		},
		"findings":            map[string]any{"type": "array", "items": finding},
		"required_actions":    map[string]any{"type": "array", "items": action},
		"unrequested_changes": stringArray,
		"residual_risks":      map[string]any{"type": "array", "items": risk},
		"assumptions":         stringArray,
	}, []string{
		"recommendation", "findings", "required_actions", "unrequested_changes",
		"residual_risks", "assumptions",
	})
}
