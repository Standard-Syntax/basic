package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	AnthropicProvider       = "anthropic"
	defaultProviderTimeout  = 5 * time.Minute
	estimatedBytesPerToken  = 4
	maximumProviderAttempts = 3
)

var (
	ErrCredentialUnavailable = errors.New("Anthropic credential unavailable")
	ErrModelUnavailable      = errors.New("Anthropic model unavailable")
	ErrContentSecret         = errors.New("provider context contains a secret")
	secretAssignment         = regexp.MustCompile(
		`(?i)(api[_-]?key|authorization|bearer|password|private[_-]?key|secret|token)\s*[:=]\s*\S+`,
	)
)

type ProviderErrorKind string

const (
	ProviderErrorAuthentication ProviderErrorKind = "authentication"
	ProviderErrorPermission     ProviderErrorKind = "permission"
	ProviderErrorBilling        ProviderErrorKind = "billing"
	ProviderErrorTimeout        ProviderErrorKind = "timeout"
	ProviderErrorRateLimit      ProviderErrorKind = "rate_limit"
	ProviderErrorRefusal        ProviderErrorKind = "refusal"
	ProviderErrorTransport      ProviderErrorKind = "transport"
	ProviderErrorResponse       ProviderErrorKind = "provider"
)

// ProviderError deliberately excludes response bodies, request headers, URLs,
// and credentials.
type ProviderError struct {
	Kind       ProviderErrorKind
	StatusCode int
	RequestID  string
	Attempts   uint32
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "Anthropic provider error"
	}
	return fmt.Sprintf("Anthropic %s error after %d attempt(s)", e.Kind, e.Attempts)
}

// CredentialSource returns a credential for one invocation. Implementations
// must not cache or log returned values.
type CredentialSource interface {
	Credential(context.Context) (string, error)
}

// CapabilityModelResolver maps a trusted manifest capability class to a
// configured provider model.
type CapabilityModelResolver interface {
	ResolveModel(context.Context, string) (string, error)
}

type StaticCapabilityModels map[string]string

func (m StaticCapabilityModels) ResolveModel(
	_ context.Context, capability string,
) (string, error) {
	model := m[capability]
	if model == "" {
		return "", ErrModelUnavailable
	}
	return model, nil
}

type MessageSender interface {
	Send(
		context.Context, string, *anthropic.MessageNewParams,
	) (*anthropic.Message, error)
}

type AnthropicOption func(*anthropicRuntime)

func WithAnthropicHTTPClient(client *http.Client) AnthropicOption {
	return func(runtime *anthropicRuntime) {
		runtime.httpClient = client
	}
}

func WithAnthropicBaseURL(baseURL string) AnthropicOption {
	return func(runtime *anthropicRuntime) {
		runtime.baseURL = baseURL
	}
}

func WithAnthropicTimeout(timeout time.Duration) AnthropicOption {
	return func(runtime *anthropicRuntime) {
		runtime.timeout = timeout
	}
}

func withAnthropicMessageSender(sender MessageSender) AnthropicOption {
	return func(runtime *anthropicRuntime) {
		runtime.sender = sender
	}
}

type anthropicRuntime struct {
	credentials CredentialSource
	models      CapabilityModelResolver
	artifacts   ArtifactStore
	httpClient  *http.Client
	baseURL     string
	sender      MessageSender
	timeout     time.Duration
	sleep       func(context.Context, time.Duration) error
}

func newAnthropicRuntime(
	credentials CredentialSource,
	models CapabilityModelResolver,
	artifacts ArtifactStore,
	options ...AnthropicOption,
) (*anthropicRuntime, error) {
	if credentials == nil || models == nil || artifacts == nil {
		return nil, errors.New(
			"Anthropic credential source, model resolver, and artifact store are required",
		)
	}
	runtime := &anthropicRuntime{
		credentials: credentials, models: models, artifacts: artifacts,
		timeout: defaultProviderTimeout, sleep: sleepContext,
	}
	for _, apply := range options {
		if apply != nil {
			apply(runtime)
		}
	}
	if runtime.timeout <= 0 {
		return nil, errors.New("positive Anthropic provider timeout is required")
	}
	if runtime.sender == nil {
		runtime.sender = &sdkMessageSender{
			httpClient: runtime.httpClient, baseURL: runtime.baseURL,
			timeout: runtime.timeout,
		}
	}
	return runtime, nil
}

type sdkMessageSender struct {
	httpClient *http.Client
	baseURL    string
	timeout    time.Duration
}

func (s *sdkMessageSender) Send(
	ctx context.Context, key string, params *anthropic.MessageNewParams,
) (*anthropic.Message, error) {
	options := []option.RequestOption{
		option.WithAPIKey(key),
		option.WithMaxRetries(0),
		option.WithRequestTimeout(s.timeout),
	}
	if s.httpClient != nil {
		options = append(options, option.WithHTTPClient(s.httpClient))
	}
	if s.baseURL != "" {
		options = append(options, option.WithBaseURL(s.baseURL))
	}
	client := anthropic.NewClient(options...)
	return client.Messages.New(ctx, *params)
}

type AnthropicImplementationAdapter struct {
	runtime *anthropicRuntime
}

func NewAnthropicImplementationAdapter(
	credentials CredentialSource,
	models CapabilityModelResolver,
	artifacts ArtifactStore,
	options ...AnthropicOption,
) (*AnthropicImplementationAdapter, error) {
	runtime, err := newAnthropicRuntime(credentials, models, artifacts, options...)
	if err != nil {
		return nil, err
	}
	return &AnthropicImplementationAdapter{runtime: runtime}, nil
}

func (a *AnthropicImplementationAdapter) ProposeImplementation(
	ctx context.Context,
	agentManifest manifest.Manifest,
	request *reasoningv1.ImplementationRequest,
) (AdapterResult, error) {
	if request == nil || request.GetEnvelope() == nil {
		return AdapterResult{}, errors.New("implementation request envelope is required")
	}
	key, model, err := a.runtime.invocationConfiguration(
		ctx, agentManifest.Model.CapabilityClass,
	)
	if err != nil {
		return AdapterResult{}, err
	}
	defer clearString(&key)
	system, user, err := a.runtime.renderImplementation(
		ctx, key, agentManifest, request,
	)
	if err != nil {
		return AdapterResult{}, err
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
			Format: anthropic.JSONOutputFormatParam{
				Schema: implementationOutputSchema(),
			},
		},
	}
	message, attempts, err := a.runtime.sendWithRetry(
		ctx, key, &params,
		request.GetEnvelope().GetBudget().GetMaximumProviderRequests(),
		request.GetEnvelope().GetExpiresAt().AsTime(),
	)
	if err != nil {
		return AdapterResult{}, err
	}
	if message.StopReason == anthropic.StopReasonRefusal {
		return AdapterResult{}, &ProviderError{
			Kind: ProviderErrorRefusal, RequestID: message.ID, Attempts: attempts,
		}
	}
	response := []byte(message.RawJSON())
	result := AdapterResult{
		ProviderResponse: response, ProviderRequestID: message.ID,
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
	projection, malformed := decodeImplementationMessage(message)
	if malformed != nil {
		result.MalformedOutput = malformed
		return result, nil
	}
	result.Proposal = implementationProposalFromProjection(projection, request)
	return result, nil
}

func (r *anthropicRuntime) sendWithRetry(
	ctx context.Context,
	key string,
	params *anthropic.MessageNewParams,
	maximumRequests uint32,
	expiresAt time.Time,
) (*anthropic.Message, uint32, error) {
	attemptLimit := min(maximumRequests, uint32(maximumProviderAttempts))
	if attemptLimit == 0 {
		return nil, 0, &ProviderError{Kind: ProviderErrorResponse}
	}
	deadline := time.Now().Add(r.timeout)
	if expiresAt.Before(deadline) {
		deadline = expiresAt
	}
	providerContext, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var lastErr error
	for attempt := uint32(1); attempt <= attemptLimit; attempt++ {
		message, err := r.sender.Send(providerContext, key, params)
		if err == nil {
			return message, attempt, nil
		}
		lastErr = err
		if providerContext.Err() != nil {
			if ctx.Err() != nil {
				return nil, attempt, ctx.Err()
			}
			return nil, attempt, &ProviderError{
				Kind: ProviderErrorTimeout, Attempts: attempt,
			}
		}
		if !retryableProviderError(err) || attempt == attemptLimit {
			return nil, attempt, classifyProviderError(err, attempt)
		}
		delay := retryDelay(err, attempt)
		if time.Now().Add(delay).After(deadline) {
			return nil, attempt, &ProviderError{
				Kind: ProviderErrorTimeout, Attempts: attempt,
			}
		}
		if err := r.sleep(providerContext, delay); err != nil {
			if ctx.Err() != nil {
				return nil, attempt, ctx.Err()
			}
			return nil, attempt, &ProviderError{
				Kind: ProviderErrorTimeout, Attempts: attempt,
			}
		}
	}
	return nil, attemptLimit, classifyProviderError(lastErr, attemptLimit)
}

func retryableProviderError(err error) bool {
	var apiError *anthropic.Error
	if errors.As(err, &apiError) {
		status := apiError.StatusCode
		return status == http.StatusRequestTimeout ||
			status == http.StatusConflict ||
			status == http.StatusTooManyRequests ||
			status >= http.StatusInternalServerError
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

func classifyProviderError(err error, attempts uint32) error {
	classified := &ProviderError{Kind: ProviderErrorTransport, Attempts: attempts}
	var apiError *anthropic.Error
	if !errors.As(err, &apiError) {
		if errors.Is(err, context.DeadlineExceeded) {
			classified.Kind = ProviderErrorTimeout
		}
		return classified
	}
	classified.StatusCode = apiError.StatusCode
	classified.RequestID = apiError.RequestID
	switch apiError.StatusCode {
	case http.StatusUnauthorized:
		classified.Kind = ProviderErrorAuthentication
	case http.StatusForbidden:
		classified.Kind = ProviderErrorPermission
	case http.StatusPaymentRequired:
		classified.Kind = ProviderErrorBilling
	case http.StatusRequestTimeout:
		classified.Kind = ProviderErrorTimeout
	case http.StatusTooManyRequests:
		classified.Kind = ProviderErrorRateLimit
	default:
		classified.Kind = ProviderErrorResponse
		if strings.Contains(strings.ToLower(string(apiError.Type())), "billing") {
			classified.Kind = ProviderErrorBilling
		}
	}
	return classified
}

func retryDelay(err error, attempt uint32) time.Duration {
	const maximumRetryAfter = 30 * time.Second
	var apiError *anthropic.Error
	if errors.As(err, &apiError) && apiError.Response != nil {
		if delay, ok := parseRetryAfter(apiError.Response.Header.Get("Retry-After")); ok &&
			delay <= maximumRetryAfter {
			return delay
		}
	}
	return time.Duration(1<<(attempt-1)) * 250 * time.Millisecond
}

func parseRetryAfter(value string) (time.Duration, bool) {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := time.Until(when)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *anthropicRuntime) invocationConfiguration(
	ctx context.Context, capability string,
) (string, string, error) {
	key, err := r.credentials.Credential(ctx)
	if err != nil || strings.TrimSpace(key) == "" {
		clearString(&key)
		return "", "", ErrCredentialUnavailable
	}
	model, err := r.models.ResolveModel(ctx, capability)
	if err != nil || strings.TrimSpace(model) == "" {
		clearString(&key)
		return "", "", ErrModelUnavailable
	}
	return key, model, nil
}

type renderedArtifact struct {
	URI     string `json:"uri"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

type renderedRepositoryFile struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

type implementationContext struct {
	Request           json.RawMessage          `json:"request"`
	RepositoryContext []renderedRepositoryFile `json:"repository_context"`
	InputArtifacts    []renderedArtifact       `json:"input_artifacts"`
}

func (r *anthropicRuntime) renderImplementation(
	ctx context.Context,
	key string,
	agentManifest manifest.Manifest,
	request *reasoningv1.ImplementationRequest,
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
	requestJSON, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(
		implementationRequestWithoutInlineContext(request),
	)
	if err != nil {
		return "", "", fmt.Errorf("render implementation request: %w", err)
	}
	contextValue := implementationContext{Request: requestJSON}
	inlineContent := make(map[string][]byte, len(request.GetRepositoryContext()))
	for _, file := range request.GetRepositoryContext() {
		content := []byte(file.GetContent())
		inlineContent[file.GetSha256()] = content
		if err := guardProviderContent(key, content); err != nil {
			return "", "", err
		}
		contextValue.RepositoryContext = append(
			contextValue.RepositoryContext,
			renderedRepositoryFile{
				Path: file.GetPath(), SHA256: file.GetSha256(), Content: file.GetContent(),
			},
		)
	}
	for _, artifact := range request.GetEnvelope().GetInputArtifacts() {
		body, readErr := r.readVerifiedArtifact(ctx, ArtifactReference{
			URI: artifact.GetArtifactUri(), SHA256: artifact.GetSha256(),
		})
		if readErr != nil {
			return "", "", fmt.Errorf("load verified input artifact: %w", readErr)
		}
		if expected, ok := inlineContent[artifact.GetSha256()]; ok {
			if !bytes.Equal(expected, body) {
				return "", "", ErrArtifactIntegrity
			}
			continue
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
		return "", "", fmt.Errorf("render implementation context: %w", err)
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

func implementationRequestWithoutInlineContext(
	request *reasoningv1.ImplementationRequest,
) *reasoningv1.ImplementationRequest {
	clone := proto.Clone(request).(*reasoningv1.ImplementationRequest)
	clone.RepositoryContext = nil
	return clone
}

func (r *anthropicRuntime) readVerifiedArtifact(
	ctx context.Context, reference ArtifactReference,
) ([]byte, error) {
	body, err := r.artifacts.Get(ctx, reference)
	if err != nil {
		return nil, err
	}
	if err := verifyArtifact(reference, body); err != nil {
		return nil, err
	}
	return body, nil
}

func guardProviderContent(key string, body []byte) error {
	text := string(body)
	if key != "" && strings.Contains(text, key) {
		return ErrContentSecret
	}
	if secretAssignment.Match(body) {
		return ErrContentSecret
	}
	return nil
}

func clearString(value *string) {
	if value != nil {
		*value = ""
	}
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func minPositive(left, right uint64) uint64 {
	if left == 0 {
		return right
	}
	if right == 0 || left < right {
		return left
	}
	return right
}

type implementationProjection struct {
	Summary                   string                 `json:"summary"`
	Changes                   []fileChangeProjection `json:"changes"`
	RequestedDeclaredCheckIDs []string               `json:"requested_declared_check_ids"`
	Assumptions               []string               `json:"assumptions"`
	UnresolvedQuestions       []string               `json:"unresolved_questions"`
	ScopeChangeRequest        *scopeChangeProjection `json:"scope_change_request"`
}

type fileChangeProjection struct {
	Path                   string   `json:"path"`
	Operation              string   `json:"operation"`
	ExpectedOriginalSHA256 string   `json:"expected_original_sha256"`
	ReplacementContent     *string  `json:"replacement_content"`
	Rationale              string   `json:"rationale"`
	AcceptanceCriterionIDs []string `json:"acceptance_criterion_ids"`
}

type scopeChangeProjection struct {
	Summary                         string   `json:"summary"`
	RequestedReadablePaths          []string `json:"requested_readable_paths"`
	RequestedWritablePaths          []string `json:"requested_writable_paths"`
	RequestedAcceptanceCriterionIDs []string `json:"requested_acceptance_criterion_ids"`
	RequestedCheckIDs               []string `json:"requested_check_ids"`
}

func decodeImplementationMessage(
	message *anthropic.Message,
) (implementationProjection, *MalformedOutput) {
	if message == nil || message.StopReason != anthropic.StopReasonEndTurn ||
		len(message.Content) != 1 || message.Content[0].Type != "text" ||
		strings.TrimSpace(message.Content[0].Text) == "" {
		return implementationProjection{}, &MalformedOutput{
			Message: "provider response must contain one complete text block",
		}
	}
	var projection implementationProjection
	decoder := json.NewDecoder(strings.NewReader(message.Content[0].Text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&projection); err != nil {
		return implementationProjection{}, &MalformedOutput{
			Message: "provider response is not valid implementation JSON",
		}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return implementationProjection{}, &MalformedOutput{
			Message: "provider response contains trailing JSON",
		}
	}
	return projection, nil
}

func implementationProposalFromProjection(
	value implementationProjection, request *reasoningv1.ImplementationRequest,
) *reasoningv1.ImplementationProposal {
	envelope := request.GetEnvelope()
	proposal := &reasoningv1.ImplementationProposal{
		Identity: &reasoningv1.ProposalIdentity{
			SchemaVersion: envelope.GetSchemaVersion(), RequestId: envelope.GetRequestId(),
			RunId: envelope.GetRunId(), TaskId: envelope.TaskId, Stage: envelope.GetStage(),
			Attempt: envelope.GetAttempt(), AgentManifestDigest: envelope.GetAgentManifestDigest(),
			InputArtifactDigests: artifactDigests(envelope.GetInputArtifacts()),
		},
		ApprovedTaskId:              request.GetApprovedTaskId(),
		ApprovedTaskDigest:          request.GetApprovedTaskDigest(),
		ApprovedSpecificationDigest: request.GetApprovedSpecificationDigest(),
		Summary:                     value.Summary, RequestedDeclaredCheckIds: value.RequestedDeclaredCheckIDs,
		Assumptions: value.Assumptions, UnresolvedQuestions: value.UnresolvedQuestions,
	}
	for _, change := range value.Changes {
		proposal.Changes = append(proposal.Changes, &reasoningv1.FileChange{
			Path: change.Path, Operation: fileOperationFromString(change.Operation),
			ExpectedOriginalSha256: change.ExpectedOriginalSHA256,
			ReplacementContent:     change.ReplacementContent, Rationale: change.Rationale,
			AcceptanceCriterionIds: change.AcceptanceCriterionIDs,
		})
	}
	if value.ScopeChangeRequest != nil {
		proposal.ScopeChangeRequest = &reasoningv1.ScopeChangeRequest{
			Summary:                         value.ScopeChangeRequest.Summary,
			RequestedReadablePaths:          value.ScopeChangeRequest.RequestedReadablePaths,
			RequestedWritablePaths:          value.ScopeChangeRequest.RequestedWritablePaths,
			RequestedAcceptanceCriterionIds: value.ScopeChangeRequest.RequestedAcceptanceCriterionIDs,
			RequestedCheckIds:               value.ScopeChangeRequest.RequestedCheckIDs,
		}
	}
	return proposal
}

func fileOperationFromString(value string) reasoningv1.FileOperation {
	switch value {
	case "create":
		return reasoningv1.FileOperation_FILE_OPERATION_CREATE
	case "update":
		return reasoningv1.FileOperation_FILE_OPERATION_UPDATE
	case "delete":
		return reasoningv1.FileOperation_FILE_OPERATION_DELETE
	default:
		return reasoningv1.FileOperation_FILE_OPERATION_UNSPECIFIED
	}
}

func implementationOutputSchema() map[string]any {
	stringArray := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	scope := closedObject(map[string]any{
		"summary":                            stringSchema(),
		"requested_readable_paths":           stringArray,
		"requested_writable_paths":           stringArray,
		"requested_acceptance_criterion_ids": stringArray,
		"requested_check_ids":                stringArray,
	}, []string{
		"summary", "requested_readable_paths", "requested_writable_paths",
		"requested_acceptance_criterion_ids", "requested_check_ids",
	})
	change := closedObject(map[string]any{
		"path":                     stringSchema(),
		"operation":                map[string]any{"type": "string", "enum": []string{"create", "update", "delete"}},
		"expected_original_sha256": stringSchema(),
		"replacement_content":      map[string]any{"type": []string{"string", "null"}},
		"rationale":                stringSchema(),
		"acceptance_criterion_ids": stringArray,
	}, []string{
		"path", "operation", "expected_original_sha256", "replacement_content",
		"rationale", "acceptance_criterion_ids",
	})
	return closedObject(map[string]any{
		"summary":                      stringSchema(),
		"changes":                      map[string]any{"type": "array", "items": change},
		"requested_declared_check_ids": stringArray,
		"assumptions":                  stringArray,
		"unresolved_questions":         stringArray,
		"scope_change_request": map[string]any{
			"anyOf": []any{scope, map[string]any{"type": "null"}},
		},
	}, []string{
		"summary", "changes", "requested_declared_check_ids", "assumptions",
		"unresolved_questions", "scope_change_request",
	})
}

func closedObject(properties map[string]any, required []string) map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": properties, "required": required,
	}
}

func stringSchema() map[string]any {
	return map[string]any{"type": "string"}
}
