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

type AnthropicPlanningAdapter struct{ runtime *anthropicRuntime }

func NewAnthropicPlanningAdapter(
	credentials CredentialSource, models CapabilityModelResolver, artifacts ArtifactStore,
	options ...AnthropicOption,
) (*AnthropicPlanningAdapter, error) {
	runtime, err := newAnthropicRuntime(credentials, models, artifacts, options...)
	if err != nil {
		return nil, err
	}
	return &AnthropicPlanningAdapter{runtime: runtime}, nil
}

func (a *AnthropicPlanningAdapter) ProposeTaskGraph(
	ctx context.Context, agentManifest manifest.Manifest, request *reasoningv1.TaskPlanningRequest,
) (PlanningAdapterResult, error) {
	if request == nil || request.GetEnvelope() == nil {
		return PlanningAdapterResult{}, errors.New("planning request envelope is required")
	}
	key, model, err := a.runtime.invocationConfiguration(ctx, agentManifest.Model.CapabilityClass)
	if err != nil {
		return PlanningAdapterResult{}, err
	}
	system, user, err := a.runtime.renderPlanning(ctx, key, agentManifest, request)
	if err != nil {
		return PlanningAdapterResult{}, err
	}
	params := anthropic.MessageNewParams{MaxTokens: int64(minPositive(
		uint64(agentManifest.Model.MaximumOutputTokens),
		request.GetEnvelope().GetBudget().GetMaximumOutputTokens())), Model: anthropic.Model(model),
		System:      []anthropic.TextBlockParam{{Text: system}},
		Messages:    []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
		Temperature: param.NewOpt(agentManifest.Model.Temperature)}
	if !a.runtime.miniMax {
		params.OutputConfig = anthropic.OutputConfigParam{Format: anthropic.JSONOutputFormatParam{
			Schema: planningOutputSchema()}}
	}
	message, attempts, err := a.runtime.sendWithRetry(ctx, key, &params,
		request.GetEnvelope().GetBudget().GetMaximumProviderRequests(),
		request.GetEnvelope().GetExpiresAt().AsTime())
	if err != nil {
		return PlanningAdapterResult{}, err
	}
	if message.StopReason == anthropic.StopReasonRefusal {
		return PlanningAdapterResult{}, &ProviderError{Kind: ProviderErrorRefusal,
			RequestID: message.ID, Attempts: attempts}
	}
	result := PlanningAdapterResult{ProviderResponse: []byte(message.RawJSON()),
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
	projection, malformed := decodePlanningMessage(message)
	if malformed != nil {
		result.MalformedOutput = malformed
		return result, nil
	}
	result.Proposal = planningProposalFromProjection(projection)
	return result, nil
}

func (r *anthropicRuntime) renderPlanning(
	ctx context.Context, key string, agentManifest manifest.Manifest,
	request *reasoningv1.TaskPlanningRequest,
) (string, string, error) {
	prompt, err := r.readVerifiedArtifact(ctx, ArtifactReference{URI: agentManifest.Prompt.ArtifactURI,
		SHA256: agentManifest.Prompt.SHA256})
	if err != nil {
		return "", "", fmt.Errorf("load verified planning prompt: %w", err)
	}
	if err := guardProviderContent(key, "planning_prompt", prompt); err != nil {
		return "", "", err
	}
	requestJSON, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(request)
	if err != nil {
		return "", "", fmt.Errorf("render planning request: %w", err)
	}
	contextValue := struct {
		Request        json.RawMessage    `json:"request"`
		InputArtifacts []renderedArtifact `json:"input_artifacts"`
	}{Request: requestJSON}
	for _, artifact := range request.GetEnvelope().GetInputArtifacts() {
		body, readErr := r.readVerifiedArtifact(ctx, ArtifactReference{URI: artifact.GetArtifactUri(),
			SHA256: artifact.GetSha256()})
		if readErr != nil {
			return "", "", fmt.Errorf("load verified planning input artifact: %w", readErr)
		}
		if err := guardProviderContent(key, "planning_artifact", body); err != nil {
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
	system, err := r.systemPrompt(prompt, planningOutputSchema())
	return system, string(user), err
}

type planningProjection struct {
	Tasks                    []plannedTaskProjection `json:"tasks"`
	Assumptions              []string                `json:"assumptions"`
	UnresolvedScopeQuestions []string                `json:"unresolved_scope_questions"`
}

type plannedTaskProjection struct {
	Objective              string   `json:"objective"`
	AcceptanceCriterionIDs []string `json:"acceptance_criterion_ids"`
	ReadablePaths          []string `json:"readable_paths"`
	WritablePaths          []string `json:"writable_paths"`
	ProhibitedPaths        []string `json:"prohibited_paths"`
	ExclusiveResources     []string `json:"exclusive_resources"`
	RequiredCheckIDs       []string `json:"required_check_ids"`
	StopConditions         []string `json:"stop_conditions"`
}

func decodePlanningMessage(message *anthropic.Message) (planningProjection, *MalformedOutput) {
	text, malformed := providerText(message)
	if malformed != nil {
		return planningProjection{}, malformed
	}
	var value planningProjection
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return planningProjection{}, malformedJSON("planning", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return planningProjection{}, &MalformedOutput{Message: "provider response contains trailing JSON",
			Kind: "trailing_json_value"}
	}
	return value, nil
}

func planningProposalFromProjection(value planningProjection) *reasoningv1.TaskGraphProposal {
	proposal := &reasoningv1.TaskGraphProposal{Assumptions: value.Assumptions,
		UnresolvedScopeQuestions: value.UnresolvedScopeQuestions}
	for _, task := range value.Tasks {
		proposal.Tasks = append(proposal.Tasks, &reasoningv1.PlannedTask{Objective: task.Objective,
			AcceptanceCriterionIds: task.AcceptanceCriterionIDs, ReadablePaths: task.ReadablePaths,
			WritablePaths: task.WritablePaths, ProhibitedPaths: task.ProhibitedPaths,
			ExclusiveResources: task.ExclusiveResources, RequiredCheckIds: task.RequiredCheckIDs,
			StopConditions: task.StopConditions})
	}
	return proposal
}

func planningOutputSchema() map[string]any {
	stringArray := map[string]any{"type": "array", "items": stringSchema()}
	task := closedObject(map[string]any{"objective": stringSchema(),
		"acceptance_criterion_ids": stringArray, "readable_paths": stringArray,
		"writable_paths": stringArray, "prohibited_paths": stringArray,
		"exclusive_resources": stringArray, "required_check_ids": stringArray,
		"stop_conditions": stringArray}, []string{"objective", "acceptance_criterion_ids",
		"readable_paths", "writable_paths", "prohibited_paths", "exclusive_resources",
		"required_check_ids", "stop_conditions"})
	return closedObject(map[string]any{"tasks": map[string]any{"type": "array", "items": task},
		"assumptions": stringArray, "unresolved_scope_questions": stringArray},
		[]string{"tasks", "assumptions", "unresolved_scope_questions"})
}
