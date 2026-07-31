package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
)

const (
	testProviderCredential = "test-minimax-credential"
	testPromptBody         = "Return exactly one proposal JSON object."
)

type loopbackProvider struct {
	mu            sync.Mutex
	server        *httptest.Server
	responseText  string
	requestBodies [][]byte
	apiKeys       []string
}

func newLoopbackProvider(t *testing.T, responseText string) *loopbackProvider {
	t.Helper()
	provider := &loopbackProvider{responseText: responseText}
	provider.server = httptest.NewServer(http.HandlerFunc(provider.serve))
	t.Cleanup(provider.server.Close)
	return provider
}

func (p *loopbackProvider) serve(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 1<<20))
	if err != nil {
		http.Error(writer, "bounded request required", http.StatusRequestEntityTooLarge)
		return
	}
	p.mu.Lock()
	p.requestBodies = append(p.requestBodies, append([]byte(nil), body...))
	p.apiKeys = append(p.apiKeys, request.Header.Get("x-api-key"))
	text := p.responseText
	p.mu.Unlock()
	response := map[string]any{
		"id": "msg_loopback", "type": "message", "role": "assistant",
		"model": MiniMaxModel, "stop_reason": "end_turn", "stop_sequence": nil,
		"content": []map[string]any{{"type": "text", "text": text}},
		"usage": map[string]any{
			"input_tokens": 11, "output_tokens": 7,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		},
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(response)
}

func (p *loopbackProvider) setResponse(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.responseText = text
}

func (p *loopbackProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requestBodies)
}

func newLoopbackImplementationAdapter(
	t *testing.T, store ArtifactStore, proposal *reasoningv1.ImplementationProposal,
) (*AnthropicImplementationAdapter, *loopbackProvider) {
	t.Helper()
	provider := newLoopbackProvider(t, implementationProjectionJSON(t, proposal))
	adapter, err := NewAnthropicImplementationAdapter(
		credentialSourceFunc(func(context.Context) (string, error) {
			return testProviderCredential, nil
		}),
		MiniMaxModels(), store,
		WithAnthropicHTTPClient(provider.server.Client()),
		WithAnthropicBaseURL(provider.server.URL),
		WithAnthropicTimeout(2*time.Second),
		WithMiniMaxCompatibility(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return adapter, provider
}

func newLoopbackReviewAdapter(
	t *testing.T, store ArtifactStore, proposal *reasoningv1.ReviewProposal,
) (*AnthropicReviewAdapter, *loopbackProvider) {
	t.Helper()
	provider := newLoopbackProvider(t, reviewProjectionJSON(t, proposal))
	adapter, err := NewAnthropicReviewAdapter(
		credentialSourceFunc(func(context.Context) (string, error) {
			return testProviderCredential, nil
		}),
		MiniMaxModels(), store,
		WithAnthropicHTTPClient(provider.server.Client()),
		WithAnthropicBaseURL(provider.server.URL),
		WithAnthropicTimeout(2*time.Second),
		WithMiniMaxCompatibility(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return adapter, provider
}

func testInputBody(index int) []byte {
	return []byte(fmt.Sprintf("bounded loopback input artifact %d", index))
}

func configureTestRequestArtifacts(
	t *testing.T, values []*reasoningv1.ArtifactDigest,
) {
	t.Helper()
	for index := range values {
		body := testInputBody(index)
		reference := referenceForBody(body)
		values[index].ArtifactUri = reference.URI
		values[index].Sha256 = reference.SHA256
	}
}

func configureImplementationRequestArtifacts(
	t *testing.T, request *reasoningv1.ImplementationRequest,
) {
	t.Helper()
	bound := make(map[string]bool)
	for _, file := range request.GetRepositoryContext() {
		bound[file.GetSha256()] = true
	}
	for index, value := range request.GetEnvelope().GetInputArtifacts() {
		if bound[value.GetSha256()] {
			continue
		}
		reference := referenceForBody(testInputBody(index))
		value.ArtifactUri = reference.URI
		value.Sha256 = reference.SHA256
	}
}

func seedTestAdapterArtifacts(
	t *testing.T, store *memoryArtifactStore, values []*reasoningv1.ArtifactDigest,
) {
	t.Helper()
	for index, value := range values {
		reference, err := store.Put(t.Context(), testInputBody(index))
		if err != nil {
			t.Fatal(err)
		}
		if reference.URI != value.GetArtifactUri() || reference.SHA256 != value.GetSha256() {
			t.Fatal("test input artifact reference is not deterministic")
		}
	}
}

func seedImplementationAdapterArtifacts(
	t *testing.T, store *memoryArtifactStore, request *reasoningv1.ImplementationRequest,
) {
	t.Helper()
	repositoryBodies := make(map[string][]byte)
	for _, file := range request.GetRepositoryContext() {
		repositoryBodies[file.GetSha256()] = []byte(file.GetContent())
	}
	for index, value := range request.GetEnvelope().GetInputArtifacts() {
		body := repositoryBodies[value.GetSha256()]
		if body == nil {
			body = testInputBody(index)
		}
		reference, err := store.Put(t.Context(), body)
		if err != nil {
			t.Fatal(err)
		}
		if reference.URI != value.GetArtifactUri() || reference.SHA256 != value.GetSha256() {
			t.Fatal("implementation input artifact reference is not deterministic")
		}
	}
}

func referenceForBody(body []byte) ArtifactReference {
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	return ArtifactReference{
		URI: "artifact://sha256/" + digest, SHA256: digest,
	}
}
