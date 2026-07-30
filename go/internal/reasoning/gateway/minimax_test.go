package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"
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
		{Mode: FakeProviderMode, Model: MiniMaxModel},
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
