package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	anthropic "github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type credentialSourceFunc func(context.Context) (string, error)

func (f credentialSourceFunc) Credential(ctx context.Context) (string, error) {
	return f(ctx)
}

type captureMessageSender struct {
	params anthropic.MessageNewParams
	key    string
	reply  *anthropic.Message
	err    error
	calls  atomic.Int32
}

type sequenceMessageSender struct {
	replies  []*anthropic.Message
	errors   []error
	position atomic.Int32
}

func (s *sequenceMessageSender) Send(
	context.Context, string, *anthropic.MessageNewParams,
) (*anthropic.Message, error) {
	index := int(s.position.Add(1)) - 1
	var reply *anthropic.Message
	var err error
	if index < len(s.replies) {
		reply = s.replies[index]
	}
	if index < len(s.errors) {
		err = s.errors[index]
	}
	return reply, err
}

func anthropicAPIError(t *testing.T, status int, retryAfter string) error {
	t.Helper()
	var apiError anthropic.Error
	if err := json.Unmarshal(
		[]byte(`{"error":{"type":"rate_limit_error"}}`), &apiError,
	); err != nil {
		t.Fatal(err)
	}
	apiError.StatusCode = status
	apiError.RequestID = "req_error"
	apiError.Response = &http.Response{StatusCode: status, Header: make(http.Header)}
	if retryAfter != "" {
		apiError.Response.Header.Set("Retry-After", retryAfter)
	}
	return &apiError
}

func (s *captureMessageSender) Send(
	_ context.Context, key string, params *anthropic.MessageNewParams,
) (*anthropic.Message, error) {
	s.calls.Add(1)
	s.key = key
	s.params = *params
	return s.reply, s.err
}

func anthropicMessage(t *testing.T, text string) *anthropic.Message {
	t.Helper()
	body := `{
		"id":"msg_123","type":"message","role":"assistant",
		"model":"claude-test","stop_reason":"end_turn","stop_sequence":null,
		"content":[{"type":"text","text":` + quotedJSON(t, text) + `}],
		"usage":{
			"input_tokens":10,"cache_creation_input_tokens":2,
			"cache_read_input_tokens":3,"output_tokens":7,
			"cache_creation":{},"inference_geo":"us",
			"output_tokens_details":{},"server_tool_use":{},"service_tier":"standard"
		}
	}`
	var message anthropic.Message
	if err := json.Unmarshal([]byte(body), &message); err != nil {
		t.Fatal(err)
	}
	return &message
}

func quotedJSON(t *testing.T, value string) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func validImplementationProjection(t *testing.T) string {
	t.Helper()
	proposal := gatewayProposal(t)
	value := implementationProjection{
		Summary:                   proposal.GetSummary(),
		RequestedDeclaredCheckIDs: proposal.GetRequestedDeclaredCheckIds(),
		Assumptions:               proposal.GetAssumptions(),
		UnresolvedQuestions:       proposal.GetUnresolvedQuestions(),
	}
	for _, change := range proposal.GetChanges() {
		value.Changes = append(value.Changes, fileChangeProjection{
			Path: change.GetPath(),
			Operation: strings.ToLower(strings.TrimPrefix(
				change.GetOperation().String(), "FILE_OPERATION_",
			)),
			ExpectedOriginalSHA256: change.GetExpectedOriginalSha256(),
			ReplacementContent:     change.ReplacementContent,
			Rationale:              change.GetRationale(),
			AcceptanceCriterionIDs: change.GetAcceptanceCriterionIds(),
		})
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func anthropicImplementationFixture(
	t *testing.T, sender MessageSender,
) (*AnthropicImplementationAdapter, manifest.Manifest, *reasoningv1.ImplementationRequest) {
	t.Helper()
	store := newMemoryArtifactStore()
	agentManifest := implementationManifest(t)
	prompt, err := store.Put(t.Context(), []byte("Return a scoped implementation proposal."))
	if err != nil {
		t.Fatal(err)
	}
	agentManifest.Prompt.ArtifactURI = prompt.URI
	agentManifest.Prompt.SHA256 = prompt.SHA256
	request := gatewayRequest(t)
	request.Envelope.ExpiresAt = timestamppb.New(time.Now().Add(time.Hour))
	request.Envelope.Budget.MaximumInputTokens = 100_000
	request.Envelope.InputArtifacts = nil
	for _, file := range request.GetRepositoryContext() {
		reference, putErr := store.Put(t.Context(), []byte(file.GetContent()))
		if putErr != nil {
			t.Fatal(putErr)
		}
		request.Envelope.InputArtifacts = append(
			request.Envelope.InputArtifacts,
			&reasoningv1.ArtifactDigest{
				ArtifactUri: reference.URI, Sha256: reference.SHA256,
			},
		)
		file.Sha256 = reference.SHA256
	}
	adapter, err := NewAnthropicImplementationAdapter(
		credentialSourceFunc(func(context.Context) (string, error) {
			return "test-credential", nil
		}),
		StaticCapabilityModels{agentManifest.Model.CapabilityClass: "claude-test"},
		store, withAnthropicMessageSender(sender),
	)
	if err != nil {
		t.Fatal(err)
	}
	return adapter, agentManifest, request
}

func TestAnthropicImplementationBuildsClosedStructuredRequest(t *testing.T) {
	sender := &captureMessageSender{reply: anthropicMessage(t, validImplementationProjection(t))}
	adapter, agentManifest, request := anthropicImplementationFixture(t, sender)
	result, err := adapter.ProposeImplementation(t.Context(), agentManifest, request)
	if err != nil {
		t.Fatal(err)
	}
	assertImplementationResult(t, result, sender)
	assertImplementationRequest(t, &sender.params, request, result.Proposal)
}

func assertImplementationResult(
	t *testing.T, result AdapterResult, sender *captureMessageSender,
) {
	t.Helper()
	if result.Proposal == nil || result.MalformedOutput != nil ||
		result.Provider != AnthropicProvider || result.ProviderRequestID != "msg_123" ||
		result.Usage.InputTokens != 15 || result.Usage.OutputTokens != 7 ||
		sender.calls.Load() != 1 || sender.key != "test-credential" {
		t.Fatalf("result=%+v calls=%d key=%q", result, sender.calls.Load(), sender.key)
	}
	if len(sender.params.System) != 1 || len(sender.params.Messages) != 1 ||
		len(sender.params.Tools) != 0 ||
		sender.params.Model != anthropic.Model("claude-test") {
		t.Fatalf("unexpected Messages request: %+v", sender.params)
	}
}

func assertImplementationRequest(
	t *testing.T,
	params *anthropic.MessageNewParams,
	request *reasoningv1.ImplementationRequest,
	proposal *reasoningv1.ImplementationProposal,
) {
	t.Helper()
	schema := params.OutputConfig.Format.Schema
	if schema["additionalProperties"] != false {
		t.Fatalf("root schema is not closed: %+v", schema)
	}
	user := params.Messages[0].Content[0].OfText.Text
	var rendered implementationContext
	if err := json.Unmarshal([]byte(user), &rendered); err != nil {
		t.Fatal(err)
	}
	if len(rendered.RepositoryContext) != 1 || len(rendered.InputArtifacts) != 0 ||
		rendered.RepositoryContext[0].Content != request.GetRepositoryContext()[0].GetContent() {
		t.Fatalf("inline repository content was duplicated: %+v", rendered)
	}
	if proposal.GetIdentity().GetRequestId() != request.GetEnvelope().GetRequestId() ||
		proposal.GetApprovedTaskDigest() != request.GetApprovedTaskDigest() {
		t.Fatal("trusted proposal identity was not injected")
	}
}

func TestAnthropicImplementationRejectsUnknownOutputFieldsDeterministically(t *testing.T) {
	sender := &captureMessageSender{reply: anthropicMessage(
		t, `{"summary":"x","unknown":true}`,
	)}
	adapter, agentManifest, request := anthropicImplementationFixture(t, sender)
	result, err := adapter.ProposeImplementation(t.Context(), agentManifest, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.MalformedOutput == nil || result.Proposal != nil ||
		len(result.ProviderResponse) == 0 {
		t.Fatalf("result=%+v", result)
	}
}

func TestAnthropicImplementationGuardsCredentialsAndContentSecrets(t *testing.T) {
	sender := &captureMessageSender{reply: anthropicMessage(t, validImplementationProjection(t))}
	adapter, agentManifest, request := anthropicImplementationFixture(t, sender)
	store := adapter.runtime.artifacts.(*memoryArtifactStore)
	prompt, err := store.Put(t.Context(), []byte("authorization: bearer leaked"))
	if err != nil {
		t.Fatal(err)
	}
	agentManifest.Prompt.ArtifactURI, agentManifest.Prompt.SHA256 = prompt.URI, prompt.SHA256
	_, err = adapter.ProposeImplementation(t.Context(), agentManifest, request)
	if !errors.Is(err, ErrContentSecret) || sender.calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, sender.calls.Load())
	}

	adapter.runtime.credentials = credentialSourceFunc(func(context.Context) (string, error) {
		return "", errors.New("credential test-credential failed")
	})
	_, err = adapter.ProposeImplementation(t.Context(), agentManifest, request)
	if !errors.Is(err, ErrCredentialUnavailable) ||
		strings.Contains(err.Error(), "test-credential") {
		t.Fatalf("credential leaked through error: %v", err)
	}
}

func TestAnthropicImplementationRetriesWithinAllBounds(t *testing.T) {
	sender := &sequenceMessageSender{
		replies: []*anthropic.Message{
			nil, nil, anthropicMessage(t, validImplementationProjection(t)),
		},
		errors: []error{
			anthropicAPIError(t, http.StatusTooManyRequests, "0"),
			anthropicAPIError(t, http.StatusInternalServerError, ""),
			nil,
		},
	}
	adapter, agentManifest, request := anthropicImplementationFixture(t, sender)
	request.Envelope.Budget.MaximumProviderRequests = 3
	var delays []time.Duration
	adapter.runtime.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}
	result, err := adapter.ProposeImplementation(t.Context(), agentManifest, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.ProviderRequests != 3 || sender.position.Load() != 3 ||
		len(delays) != 2 || delays[0] != 0 || delays[1] != 500*time.Millisecond {
		t.Fatalf(
			"usage=%+v calls=%d delays=%v",
			result.Usage, sender.position.Load(), delays,
		)
	}
}

func TestAnthropicImplementationAllowsUnsetExpiry(t *testing.T) {
	sender := &captureMessageSender{
		reply: anthropicMessage(t, validImplementationProjection(t)),
	}
	adapter, agentManifest, request := anthropicImplementationFixture(t, sender)
	request.Envelope.ExpiresAt = nil
	result, err := adapter.ProposeImplementation(t.Context(), agentManifest, request)
	if err != nil || result.Proposal == nil || sender.calls.Load() != 1 {
		t.Fatalf("result=%+v calls=%d err=%v", result, sender.calls.Load(), err)
	}
}

func TestAnthropicRetryAfterIsCapped(t *testing.T) {
	err := anthropicAPIError(t, http.StatusTooManyRequests, "120")
	if delay := retryDelay(err, 1); delay != 30*time.Second {
		t.Fatalf("retry delay = %s", delay)
	}
}

func TestAnthropicImplementationClassifiesProviderFailuresWithoutLeaks(t *testing.T) {
	sender := &sequenceMessageSender{
		errors: []error{anthropicAPIError(t, http.StatusUnauthorized, "")},
	}
	adapter, agentManifest, request := anthropicImplementationFixture(t, sender)
	_, err := adapter.ProposeImplementation(t.Context(), agentManifest, request)
	var providerError *ProviderError
	if !errors.As(err, &providerError) ||
		providerError.Kind != ProviderErrorAuthentication ||
		providerError.Attempts != 1 || strings.Contains(err.Error(), "test-credential") {
		t.Fatalf("err=%v", err)
	}
}

func TestAnthropicImplementationRefusalAndUsageBudgetAreNotAccepted(t *testing.T) {
	t.Run("refusal", func(t *testing.T) {
		message := anthropicMessage(t, validImplementationProjection(t))
		message.StopReason = anthropic.StopReasonRefusal
		sender := &captureMessageSender{reply: message}
		adapter, agentManifest, request := anthropicImplementationFixture(t, sender)
		_, err := adapter.ProposeImplementation(t.Context(), agentManifest, request)
		var providerError *ProviderError
		if !errors.As(err, &providerError) ||
			providerError.Kind != ProviderErrorRefusal {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("usage", func(t *testing.T) {
		message := anthropicMessage(t, validImplementationProjection(t))
		message.Usage.InputTokens = 100_001
		sender := &captureMessageSender{reply: message}
		adapter, agentManifest, request := anthropicImplementationFixture(t, sender)
		result, err := adapter.ProposeImplementation(t.Context(), agentManifest, request)
		if err != nil || result.MalformedOutput == nil || result.Proposal != nil {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
}

func TestAnthropicImplementationHonorsCallerCancellation(t *testing.T) {
	sender := &sequenceMessageSender{
		errors: []error{anthropicAPIError(t, http.StatusInternalServerError, "")},
	}
	adapter, agentManifest, request := anthropicImplementationFixture(t, sender)
	adapter.runtime.sleep = func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := adapter.ProposeImplementation(ctx, agentManifest, request)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}
