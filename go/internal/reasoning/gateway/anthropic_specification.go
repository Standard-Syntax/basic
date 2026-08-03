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

type AnthropicSpecificationAdapter struct{ runtime *anthropicRuntime }

func NewAnthropicSpecificationAdapter(
	credentials CredentialSource, models CapabilityModelResolver, artifacts ArtifactStore,
	options ...AnthropicOption,
) (*AnthropicSpecificationAdapter, error) {
	runtime, err := newAnthropicRuntime(credentials, models, artifacts, options...)
	if err != nil {
		return nil, err
	}
	return &AnthropicSpecificationAdapter{runtime: runtime}, nil
}

func (a *AnthropicSpecificationAdapter) ProposeSpecification(
	ctx context.Context, agentManifest manifest.Manifest, request *reasoningv1.SpecificationRequest,
) (SpecificationAdapterResult, error) {
	if request == nil || request.GetEnvelope() == nil {
		return SpecificationAdapterResult{}, errors.New("specification request envelope is required")
	}
	key, model, err := a.runtime.invocationConfiguration(ctx, agentManifest.Model.CapabilityClass)
	if err != nil {
		return SpecificationAdapterResult{}, err
	}
	system, user, err := a.runtime.renderSpecification(ctx, key, agentManifest, request)
	if err != nil {
		return SpecificationAdapterResult{}, err
	}
	params := anthropic.MessageNewParams{MaxTokens: int64(minPositive(
		uint64(agentManifest.Model.MaximumOutputTokens),
		request.GetEnvelope().GetBudget().GetMaximumOutputTokens())),
		Model: anthropic.Model(model), System: []anthropic.TextBlockParam{{Text: system}},
		Messages:    []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
		Temperature: param.NewOpt(agentManifest.Model.Temperature)}
	if !a.runtime.miniMax {
		params.OutputConfig = anthropic.OutputConfigParam{Format: anthropic.JSONOutputFormatParam{
			Schema: specificationOutputSchema()}}
	}
	message, attempts, err := a.runtime.sendWithRetry(ctx, key, &params,
		request.GetEnvelope().GetBudget().GetMaximumProviderRequests(),
		request.GetEnvelope().GetExpiresAt().AsTime())
	if err != nil {
		return SpecificationAdapterResult{}, err
	}
	if message.StopReason == anthropic.StopReasonRefusal {
		return SpecificationAdapterResult{}, &ProviderError{Kind: ProviderErrorRefusal,
			RequestID: message.ID, Attempts: attempts}
	}
	result := SpecificationAdapterResult{ProviderResponse: []byte(message.RawJSON()),
		ProviderRequestID: message.ID, Provider: a.runtime.providerName(), Model: string(message.Model),
		Usage: Usage{InputTokens: uint64(maxInt64(0, message.Usage.InputTokens+
			message.Usage.CacheCreationInputTokens+message.Usage.CacheReadInputTokens)),
			OutputTokens: uint64(maxInt64(0, message.Usage.OutputTokens)), ProviderRequests: attempts}}
	budget := request.GetEnvelope().GetBudget()
	if result.Usage.InputTokens > budget.GetMaximumInputTokens() ||
		result.Usage.OutputTokens > budget.GetMaximumOutputTokens() {
		result.MalformedOutput = &MalformedOutput{Message: "provider token usage exceeds the trusted request budget"}
		return result, nil
	}
	projection, malformed := decodeSpecificationMessage(message)
	if malformed != nil {
		result.MalformedOutput = malformed
		return result, nil
	}
	result.Proposal = specificationProposalFromProjection(projection, request)
	return result, nil
}

func (r *anthropicRuntime) renderSpecification(
	ctx context.Context, key string, agentManifest manifest.Manifest,
	request *reasoningv1.SpecificationRequest,
) (string, string, error) {
	prompt, err := r.readVerifiedArtifact(ctx, ArtifactReference{URI: agentManifest.Prompt.ArtifactURI,
		SHA256: agentManifest.Prompt.SHA256})
	if err != nil {
		return "", "", fmt.Errorf("load verified specification prompt: %w", err)
	}
	if err := guardProviderContent(key, "specification_prompt", prompt); err != nil {
		return "", "", err
	}
	requestJSON, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(request)
	if err != nil {
		return "", "", fmt.Errorf("render specification request: %w", err)
	}
	contextValue := struct {
		Request        json.RawMessage    `json:"request"`
		InputArtifacts []renderedArtifact `json:"input_artifacts"`
	}{Request: requestJSON}
	for _, artifact := range request.GetEnvelope().GetInputArtifacts() {
		body, readErr := r.readVerifiedArtifact(ctx, ArtifactReference{URI: artifact.GetArtifactUri(),
			SHA256: artifact.GetSha256()})
		if readErr != nil {
			return "", "", fmt.Errorf("load verified specification input artifact: %w", readErr)
		}
		if err := guardProviderContent(key, "specification_artifact", body); err != nil {
			return "", "", err
		}
		contextValue.InputArtifacts = append(contextValue.InputArtifacts, renderedArtifact{
			URI: artifact.GetArtifactUri(), SHA256: artifact.GetSha256(), Content: string(body)})
	}
	user, err := json.Marshal(contextValue)
	if err != nil {
		return "", "", err
	}
	maximumTokens := minPositive(uint64(agentManifest.Context.MaximumContextTokens),
		request.GetEnvelope().GetBudget().GetMaximumInputTokens())
	if maximumTokens == 0 || uint64((len(prompt)+len(user)+estimatedBytesPerToken-1)/estimatedBytesPerToken) > maximumTokens {
		return "", "", errors.New("rendered provider context exceeds input token budget")
	}
	system, err := r.systemPrompt(prompt, specificationOutputSchema())
	return system, string(user), err
}

type specificationProjection struct {
	Title              string                             `json:"title"`
	Goal               string                             `json:"goal"`
	Actors             []string                           `json:"actors"`
	Constraints        []string                           `json:"constraints"`
	NonGoals           []string                           `json:"non_goals"`
	AcceptanceCriteria []specificationCriterionProjection `json:"acceptance_criteria"`
	Assumptions        []string                           `json:"assumptions"`
	Risks              []specificationRiskProjection      `json:"risks"`
	Questions          []specificationQuestionProjection  `json:"questions"`
}

type specificationCriterionProjection struct {
	CriterionID        string `json:"criterion_id"`
	Description        string `json:"description"`
	VerificationMethod string `json:"verification_method"`
}
type specificationRiskProjection struct {
	RiskID      string `json:"risk_id"`
	Description string `json:"description"`
	Mitigation  string `json:"mitigation"`
}
type specificationQuestionProjection struct {
	QuestionID string `json:"question_id"`
	Question   string `json:"question"`
	Blocking   bool   `json:"blocking"`
}

func decodeSpecificationMessage(message *anthropic.Message) (specificationProjection, *MalformedOutput) {
	text, malformed := providerText(message)
	if malformed != nil {
		return specificationProjection{}, malformed
	}
	var value specificationProjection
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return specificationProjection{}, malformedJSON("specification", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return specificationProjection{}, &MalformedOutput{Message: "provider response contains trailing JSON", Kind: "trailing_json_value"}
	}
	return value, nil
}

func specificationProposalFromProjection(
	value specificationProjection, request *reasoningv1.SpecificationRequest,
) *reasoningv1.SpecificationProposal {
	envelope := request.GetEnvelope()
	proposal := &reasoningv1.SpecificationProposal{Identity: &reasoningv1.ProposalIdentity{
		SchemaVersion: envelope.GetSchemaVersion(), RequestId: envelope.GetRequestId(),
		RunId: envelope.GetRunId(), TaskId: envelope.TaskId, Stage: envelope.GetStage(),
		Attempt: envelope.GetAttempt(), AgentManifestDigest: envelope.GetAgentManifestDigest(),
		InputArtifactDigests: artifactDigests(envelope.GetInputArtifacts())},
		Title: value.Title, Goal: value.Goal, Actors: value.Actors, Constraints: value.Constraints,
		NonGoals: value.NonGoals, Assumptions: value.Assumptions}
	for _, criterion := range value.AcceptanceCriteria {
		proposal.AcceptanceCriteria = append(proposal.AcceptanceCriteria, &reasoningv1.AcceptanceCriterion{
			CriterionId: criterion.CriterionID, Description: criterion.Description,
			VerificationMethod: criterion.VerificationMethod})
	}
	for _, risk := range value.Risks {
		proposal.Risks = append(proposal.Risks, &reasoningv1.SpecificationRisk{
			RiskId: risk.RiskID, Description: risk.Description, Mitigation: risk.Mitigation})
	}
	for _, question := range value.Questions {
		proposal.Questions = append(proposal.Questions, &reasoningv1.SpecificationQuestion{
			QuestionId: question.QuestionID, Question: question.Question, Blocking: question.Blocking})
	}
	return proposal
}

func specificationOutputSchema() map[string]any {
	stringArray := map[string]any{"type": "array", "items": stringSchema()}
	criterion := closedObject(map[string]any{"criterion_id": stringSchema(), "description": stringSchema(),
		"verification_method": stringSchema()}, []string{"criterion_id", "description", "verification_method"})
	risk := closedObject(map[string]any{"risk_id": stringSchema(), "description": stringSchema(),
		"mitigation": stringSchema()}, []string{"risk_id", "description", "mitigation"})
	question := closedObject(map[string]any{"question_id": stringSchema(), "question": stringSchema(),
		"blocking": map[string]any{"type": "boolean"}}, []string{"question_id", "question", "blocking"})
	return closedObject(map[string]any{"title": stringSchema(), "goal": stringSchema(), "actors": stringArray,
		"constraints": stringArray, "non_goals": stringArray,
		"acceptance_criteria": map[string]any{"type": "array", "items": criterion},
		"assumptions":         stringArray, "risks": map[string]any{"type": "array", "items": risk},
		"questions": map[string]any{"type": "array", "items": question}},
		[]string{"title", "goal", "actors", "constraints", "non_goals", "acceptance_criteria", "assumptions", "risks", "questions"})
}
