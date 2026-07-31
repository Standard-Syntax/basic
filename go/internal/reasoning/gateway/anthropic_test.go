package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
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
	return implementationProjectionJSON(t, gatewayProposal(t))
}

func implementationProjectionJSON(
	t *testing.T, proposal *reasoningv1.ImplementationProposal,
) string {
	t.Helper()
	value := implementationProjection{
		Summary:                   proposal.GetSummary(),
		RequestedDeclaredCheckIDs: proposal.GetRequestedDeclaredCheckIds(),
		Assumptions:               proposal.GetAssumptions(),
		UnresolvedQuestions:       proposal.GetUnresolvedQuestions(),
	}
	if scope := proposal.GetScopeChangeRequest(); scope != nil {
		value.ScopeChangeRequest = &scopeChangeProjection{
			Summary:                         scope.GetSummary(),
			RequestedReadablePaths:          scope.GetRequestedReadablePaths(),
			RequestedWritablePaths:          scope.GetRequestedWritablePaths(),
			RequestedAcceptanceCriterionIDs: scope.GetRequestedAcceptanceCriterionIds(),
			RequestedCheckIDs:               scope.GetRequestedCheckIds(),
		}
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
	mappedRequest, err := contracts.MapImplementationRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	mappedProposal, err := contracts.MapImplementationProposal(result.Proposal, mappedRequest)
	if err != nil {
		t.Fatalf("proposal failed unchanged kernel validator: %v", err)
	}
	expected := gatewayProposal(t)
	if len(mappedProposal.Changes) != len(expected.GetChanges()) ||
		len(mappedProposal.RequestedDeclaredCheckIDs) != 1 ||
		mappedProposal.RequestedDeclaredCheckIDs[0] != request.GetAvailableCheckIds()[0] {
		t.Fatalf("mapped proposal lost scoped changes or declared checks: %+v", mappedProposal)
	}
	for index, change := range result.Proposal.GetChanges() {
		if change.GetPath() != expected.GetChanges()[index].GetPath() ||
			change.GetReplacementContent() != expected.GetChanges()[index].GetReplacementContent() ||
			change.GetExpectedOriginalSha256() !=
				expected.GetChanges()[index].GetExpectedOriginalSha256() ||
			len(change.GetAcceptanceCriterionIds()) == 0 {
			t.Fatalf("change %d lost complete-file, digest, or criterion data: %+v", index, change)
		}
	}
}

func TestAnthropicImplementationAcceptsDocumentedThinkingBeforeOneTextBlock(t *testing.T) {
	message := anthropicMessage(t, validImplementationProjection(t))
	message.Content = append([]anthropic.ContentBlockUnion{{
		Type: "thinking", Thinking: "untrusted reasoning", Signature: "fixture-signature",
	}}, message.Content...)
	sender := &captureMessageSender{reply: message}
	adapter, agentManifest, request := anthropicImplementationFixture(t, sender)

	result, err := adapter.ProposeImplementation(t.Context(), agentManifest, request)
	if err != nil || result.MalformedOutput != nil || result.Proposal == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestAnthropicImplementationRejectsUnexpectedOrMultipleContentBlocks(t *testing.T) {
	thinking := anthropic.ContentBlockUnion{
		Type: "thinking", Thinking: "untrusted reasoning", Signature: "fixture-signature",
	}
	for _, test := range []struct {
		name   string
		mutate func(*anthropic.Message)
	}{
		{name: "second text", mutate: func(message *anthropic.Message) {
			message.Content = append(
				message.Content, anthropic.ContentBlockUnion{Type: "text", Text: "{}"},
			)
		}},
		{name: "tool use", mutate: func(message *anthropic.Message) {
			message.Content = append(
				message.Content, anthropic.ContentBlockUnion{Type: "tool_use", ID: "tool-1"},
			)
		}},
		{name: "thinking after text", mutate: func(message *anthropic.Message) {
			message.Content = append(message.Content, thinking)
		}},
		{name: "duplicate thinking", mutate: func(message *anthropic.Message) {
			message.Content = append([]anthropic.ContentBlockUnion{thinking, thinking}, message.Content...)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := anthropicMessage(t, validImplementationProjection(t))
			test.mutate(message)
			sender := &captureMessageSender{reply: message}
			adapter, agentManifest, request := anthropicImplementationFixture(t, sender)
			result, err := adapter.ProposeImplementation(t.Context(), agentManifest, request)
			if err != nil || result.MalformedOutput == nil || result.Proposal != nil {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
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

func TestAnthropicImplementationPreservesScopeChangeOnlyProjection(t *testing.T) {
	value := implementationProjection{
		Summary:             "Approved scope is insufficient for a safe proposal.",
		UnresolvedQuestions: []string{"May the generated package be changed?"},
		ScopeChangeRequest: &scopeChangeProjection{
			Summary:                "The required generated file is outside writable scope.",
			RequestedReadablePaths: []string{"go/gen"},
			RequestedWritablePaths: []string{"go/gen"},
			RequestedCheckIDs:      []string{"CHECK-GO-TEST"},
		},
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sender := &captureMessageSender{reply: anthropicMessage(t, string(body))}
	adapter, agentManifest, request := anthropicImplementationFixture(t, sender)
	result, err := adapter.ProposeImplementation(t.Context(), agentManifest, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Proposal == nil || len(result.Proposal.GetChanges()) != 0 ||
		result.Proposal.GetScopeChangeRequest().GetRequestedWritablePaths()[0] != "go/gen" ||
		result.Proposal.GetIdentity().GetRequestId() != request.GetEnvelope().GetRequestId() {
		t.Fatalf("scope-change-only projection was not faithfully mapped: %+v", result)
	}
	mappedRequest, err := contracts.MapImplementationRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contracts.MapImplementationProposal(result.Proposal, mappedRequest); err == nil {
		t.Fatal("incomplete v1 scope-change-only proposal unexpectedly advanced kernel validation")
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

func TestAnthropicImplementationExhaustsTimeoutAndTransportRetries(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind ProviderErrorKind
	}{
		{
			name: "provider timeout",
			err:  anthropicAPIError(t, http.StatusRequestTimeout, ""),
			kind: ProviderErrorTimeout,
		},
		{
			name: "transport failure",
			err: &net.OpError{
				Op: "dial", Net: "tcp", Err: errors.New("fixture transport unavailable"),
			},
			kind: ProviderErrorTransport,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sender := &sequenceMessageSender{errors: []error{test.err, test.err, test.err}}
			adapter, agentManifest, request := anthropicImplementationFixture(t, sender)
			request.Envelope.Budget.MaximumProviderRequests = 3
			adapter.runtime.sleep = func(context.Context, time.Duration) error { return nil }

			_, err := adapter.ProposeImplementation(t.Context(), agentManifest, request)
			var providerError *ProviderError
			if !errors.As(err, &providerError) || providerError.Kind != test.kind ||
				providerError.Attempts != 3 || sender.position.Load() != 3 {
				t.Fatalf("err=%v calls=%d", err, sender.position.Load())
			}
		})
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

func TestAnthropicLoopbackRequestHeadersShapeAndRetry(t *testing.T) {
	var (
		mu      sync.Mutex
		bodies  [][]byte
		headers []http.Header
	)
	reply := anthropicMessage(t, validImplementationProjection(t))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		headers = append(headers, request.Header.Clone())
		call := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"bounded"}}`))
			return
		}
		_, _ = w.Write([]byte(reply.RawJSON()))
	}))
	defer server.Close()

	adapter, agentManifest, request := anthropicImplementationFixture(t, nil)
	request.Envelope.Budget.MaximumProviderRequests = 2
	adapter.runtime.sender = &sdkMessageSender{
		baseURL: server.URL, timeout: time.Minute, httpClient: server.Client(),
	}
	adapter.runtime.sleep = func(context.Context, time.Duration) error { return nil }
	result, err := adapter.ProposeImplementation(t.Context(), agentManifest, request)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || result.Usage.ProviderRequests != 2 {
		t.Fatalf("requests=%d usage=%+v", len(bodies), result.Usage)
	}
	for _, header := range headers {
		if header.Get("X-Api-Key") != "test-credential" ||
			header.Get("Anthropic-Version") == "" {
			t.Fatalf("Anthropic headers missing: %+v", header)
		}
	}
	var body map[string]any
	if err := json.Unmarshal(bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "claude-test" || body["tools"] != nil ||
		body["output_config"] == nil || len(body["messages"].([]any)) != 1 {
		t.Fatalf("unexpected Anthropic request body: %+v", body)
	}
}

func TestAnthropicGatewayExactReplaySkipsCredentialAndNetwork(t *testing.T) {
	sender := &captureMessageSender{reply: anthropicMessage(t, validImplementationProjection(t))}
	adapter, agentManifest, request := anthropicImplementationFixture(t, sender)
	var credentialCalls atomic.Int32
	adapter.runtime.credentials = credentialSourceFunc(func(context.Context) (string, error) {
		credentialCalls.Add(1)
		return "replay-test-credential", nil
	})
	resolver := &fakeResolver{resolved: ResolvedManifest{
		Digest: request.GetEnvelope().GetAgentManifestDigest(), Manifest: agentManifest,
	}}
	service, err := NewService(
		resolver, adapter, adapter.runtime.artifacts, newMemoryInvocationRepository(),
		fixedClock{now: time.Now()},
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.ProposeImplementation(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ProposeImplementation(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replay || !second.Replay || credentialCalls.Load() != 1 ||
		sender.calls.Load() != 1 || resolver.calls.Load() != 1 ||
		first.ProviderResponseArtifact != second.ProviderResponseArtifact {
		t.Fatalf(
			"first=%+v second=%+v credential=%d network=%d resolver=%d",
			first, second, credentialCalls.Load(), sender.calls.Load(), resolver.calls.Load(),
		)
	}
}
