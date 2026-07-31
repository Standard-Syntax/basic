package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestMiniMaxProviderConfigurationIsClosed(t *testing.T) {
	got, err := (ProviderConfig{}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != MiniMaxMode || got.BaseURL != MiniMaxBaseURL ||
		got.Model != MiniMaxModel || got.APIKeyEnv != MiniMaxAPIKeyEnv {
		t.Fatalf("defaults = %#v", got)
	}
	for _, value := range []ProviderConfig{
		{Mode: "other"},
		{Mode: MiniMaxMode, BaseURL: "http://api.minimax.io/anthropic"},
		{Mode: MiniMaxMode, BaseURL: "https://example.invalid"},
		{Mode: MiniMaxMode, Model: "other"},
		{Mode: MiniMaxMode, APIKeyEnv: "OTHER_KEY"},
		{Mode: "fake"},
	} {
		if _, err := value.Normalize(); err == nil {
			t.Fatalf("accepted provider config %#v", value)
		}
	}
}

func TestMiniMaxCredentialReadsEnvironmentEveryInvocation(t *testing.T) {
	source := EnvironmentCredentialSource{Name: MiniMaxAPIKeyEnv}
	t.Setenv(MiniMaxAPIKeyEnv, "first")
	first, err := source.Credential(t.Context())
	if err != nil || first != "first" {
		t.Fatalf("first = %q, %v", first, err)
	}
	t.Setenv(MiniMaxAPIKeyEnv, "second")
	second, err := source.Credential(t.Context())
	if err != nil || second != "second" {
		t.Fatalf("second = %q, %v", second, err)
	}
	if err := os.Unsetenv(MiniMaxAPIKeyEnv); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Credential(context.Background()); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("missing credential = %v", err)
	}
}

func TestMiniMaxImplementationOmitsUnsupportedOutputConfig(t *testing.T) {
	message := anthropicMessage(t, validImplementationProjection(t))
	message.Model = anthropic.Model(MiniMaxModel)
	sender := &captureMessageSender{reply: message}
	adapter, agentManifest, request := anthropicImplementationFixture(t, sender)
	adapter.runtime.miniMax = true
	adapter.runtime.models = StaticCapabilityModels{
		agentManifest.Model.CapabilityClass: MiniMaxModel,
	}
	result, err := adapter.ProposeImplementation(t.Context(), agentManifest, request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(sender.params)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "output_config") ||
		strings.Contains(string(body), `"thinking"`) ||
		strings.Contains(string(body), `"tools"`) ||
		!strings.Contains(sender.params.System[0].Text, "exactly one JSON object") ||
		!strings.Contains(sender.params.System[0].Text, `"additionalProperties":false`) {
		t.Fatalf("MiniMax request = %s system=%q", body, sender.params.System[0].Text)
	}
	if result.Provider != MiniMaxAnthropicProvider || result.Model != MiniMaxModel {
		t.Fatalf("metadata = provider %q model %q", result.Provider, result.Model)
	}
}

func TestMiniMaxAdapterReadsChangedEnvironmentCredentialPerInvocation(t *testing.T) {
	store := newMemoryArtifactStore()
	request := gatewayRequest(t)
	request.Envelope.ExpiresAt = timestamppb.New(time.Now().Add(time.Hour))
	seedImplementationAdapterArtifacts(t, store, request)
	agentManifest := implementationManifest(t)
	prompt, err := store.Put(t.Context(), []byte(testPromptBody))
	if err != nil {
		t.Fatal(err)
	}
	agentManifest.Prompt.ArtifactURI = prompt.URI
	agentManifest.Prompt.SHA256 = prompt.SHA256
	provider := newLoopbackProvider(t, implementationProjectionJSON(t, gatewayProposal(t)))
	adapter, err := NewAnthropicImplementationAdapter(
		EnvironmentCredentialSource{Name: MiniMaxAPIKeyEnv},
		MiniMaxModels(), store,
		WithAnthropicHTTPClient(provider.server.Client()),
		WithAnthropicBaseURL(provider.server.URL),
		WithAnthropicTimeout(2*time.Second),
		WithMiniMaxCompatibility(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(MiniMaxAPIKeyEnv, "first-invocation-key")
	if _, err := adapter.ProposeImplementation(t.Context(), agentManifest, request); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MiniMaxAPIKeyEnv, "second-invocation-key")
	if _, err := adapter.ProposeImplementation(t.Context(), agentManifest, request); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.apiKeys) != 2 ||
		provider.apiKeys[0] != "first-invocation-key" ||
		provider.apiKeys[1] != "second-invocation-key" {
		t.Fatalf("per-invocation credential headers = %#v", provider.apiKeys)
	}
	for _, body := range provider.requestBodies {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != MiniMaxModel || payload["tools"] != nil ||
			payload["thinking"] != nil || payload["output_config"] != nil {
			t.Fatalf("MiniMax loopback request = %s", body)
		}
	}
}
