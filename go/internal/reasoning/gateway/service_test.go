package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"google.golang.org/protobuf/proto"
)

func gatewayFixture(t *testing.T, values ...string) []byte {
	t.Helper()
	parts := append([]string{"..", "..", "..", "..", "tests", "contracts", "v1"}, values...)
	data, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func gatewayRequest(t *testing.T) *reasoningv1.ImplementationRequest {
	t.Helper()
	var request reasoningv1.ImplementationRequest
	if err := proto.Unmarshal(
		gatewayFixture(t, "implementation", "request.bin"), &request,
	); err != nil {
		t.Fatal(err)
	}
	configureImplementationRequestArtifacts(t, &request)
	return &request
}

func gatewayProposal(t *testing.T) *reasoningv1.ImplementationProposal {
	t.Helper()
	var proposal reasoningv1.ImplementationProposal
	if err := proto.Unmarshal(
		gatewayFixture(t, "implementation", "proposal.bin"), &proposal,
	); err != nil {
		t.Fatal(err)
	}
	return &proposal
}

func implementationManifest(t *testing.T) manifest.Manifest {
	t.Helper()
	value, _, _, err := manifest.Read(gatewayFixture(t, "manifest", "implementation.json"))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time {
	return c.now
}

type fakeResolver struct {
	resolved ResolvedManifest
	err      error
	calls    atomic.Int32
}

func (r *fakeResolver) ResolveManifest(
	_ context.Context, _ string,
) (ResolvedManifest, error) {
	r.calls.Add(1)
	return r.resolved, r.err
}

type countingAdapter struct {
	inner    ImplementationAdapter
	provider *loopbackProvider
	err      error
	calls    atomic.Int32
}

type implementationAdapterFunc func(
	context.Context, manifest.Manifest, *reasoningv1.ImplementationRequest,
) (AdapterResult, error)

func (f implementationAdapterFunc) ProposeImplementation(
	ctx context.Context, value manifest.Manifest, request *reasoningv1.ImplementationRequest,
) (AdapterResult, error) {
	return f(ctx, value, request)
}

type memoryArtifactStore struct {
	mu      sync.Mutex
	values  map[string][]byte
	puts    int
	failPut int
}

func newMemoryArtifactStore() *memoryArtifactStore {
	return &memoryArtifactStore{values: make(map[string][]byte)}
}

func (s *memoryArtifactStore) Put(
	_ context.Context, body []byte,
) (ArtifactReference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	if s.puts == s.failPut {
		return ArtifactReference{}, errors.New("injected artifact put failure")
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	s.values[digest] = append([]byte(nil), body...)
	return ArtifactReference{
		URI: "artifact://sha256/" + digest, SHA256: digest,
	}, nil
}

func (s *memoryArtifactStore) Get(
	_ context.Context, reference ArtifactReference,
) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.values[reference.SHA256]
	if !ok {
		return nil, errors.New("artifact not found")
	}
	if err := verifyArtifact(reference, body); err != nil {
		return nil, err
	}
	return append([]byte(nil), body...), nil
}

type memoryInvocationRepository struct {
	mu      sync.Mutex
	records map[string]InvocationRecord
}

func newMemoryInvocationRepository() *memoryInvocationRepository {
	return &memoryInvocationRepository{records: make(map[string]InvocationRecord)}
}

func (r *memoryInvocationRepository) Begin(
	_ context.Context, start InvocationStart,
) (InvocationHandle, error) {
	r.mu.Lock()
	if record, ok := r.records[start.RequestID]; ok {
		r.mu.Unlock()
		if record.RequestArtifact.SHA256 != start.RequestArtifact.SHA256 {
			return nil, ErrInvocationConflict
		}
		return &memoryInvocationHandle{replay: &record}, nil
	}
	return &memoryInvocationHandle{repository: r, start: start, locked: true}, nil
}

type memoryInvocationHandle struct {
	repository *memoryInvocationRepository
	start      InvocationStart
	replay     *InvocationRecord
	locked     bool
}

func (h *memoryInvocationHandle) Replay() (InvocationRecord, bool) {
	if h.replay == nil {
		return InvocationRecord{}, false
	}
	return *h.replay, true
}

func (h *memoryInvocationHandle) Complete(
	_ context.Context, completion InvocationCompletion,
) (InvocationRecord, error) {
	if !h.locked || h.repository == nil {
		return InvocationRecord{}, ErrInvocationState
	}
	record := InvocationRecord{
		InvocationStart: h.start, InvocationCompletion: completion,
	}
	h.repository.records[h.start.RequestID] = record
	h.locked = false
	h.repository.mu.Unlock()
	return record, nil
}

func (h *memoryInvocationHandle) Rollback(_ context.Context) error {
	if h.locked {
		h.locked = false
		h.repository.mu.Unlock()
	}
	return nil
}

func (a *countingAdapter) ProposeImplementation(
	ctx context.Context, value manifest.Manifest, request *reasoningv1.ImplementationRequest,
) (AdapterResult, error) {
	a.calls.Add(1)
	if a.err != nil {
		return AdapterResult{}, a.err
	}
	return a.inner.ProposeImplementation(ctx, value, request)
}

func gatewayService(
	t *testing.T, template *reasoningv1.ImplementationProposal,
) (*Service, *fakeResolver, *countingAdapter) {
	t.Helper()
	request := gatewayRequest(t)
	resolver := &fakeResolver{resolved: ResolvedManifest{
		Digest: request.GetEnvelope().GetAgentManifestDigest(),
	}}
	store := newMemoryArtifactStore()
	seedImplementationAdapterArtifacts(t, store, request)
	agentManifest := implementationManifest(t)
	prompt, err := store.Put(t.Context(), []byte(testPromptBody))
	if err != nil {
		t.Fatal(err)
	}
	agentManifest.Prompt.ArtifactURI = prompt.URI
	agentManifest.Prompt.SHA256 = prompt.SHA256
	resolver.resolved.Manifest = agentManifest
	production, provider := newLoopbackImplementationAdapter(t, store, template)
	adapter := &countingAdapter{inner: production, provider: provider}
	store.puts = 0
	service, err := NewService(
		resolver, adapter, store, newMemoryInvocationRepository(),
		fixedClock{now: request.GetEnvelope().GetCreatedAt().AsTime().Add(time.Minute)},
	)
	if err != nil {
		t.Fatal(err)
	}
	return service, resolver, adapter
}

func TestMiniMaxImplementationReturnsOneValidatedProposalAndExactReplay(t *testing.T) {
	template := gatewayProposal(t)
	service, resolver, adapter := gatewayService(t, template)
	request := gatewayRequest(t)

	first, err := service.ProposeImplementation(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ProposeImplementation(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	requireProposalOutcomes(t, first, second)
	requireDeterministicProposal(t, first.Proposal, second.Proposal, request)
	requireMiniMaxInvocationMetadata(t, first, resolver, adapter)
}

func requireProposalOutcomes(t *testing.T, first, second Outcome) {
	t.Helper()
	if first.Proposal == nil || first.Rejection != nil ||
		second.Proposal == nil || second.Rejection != nil {
		t.Fatalf("outcomes are not proposal-only: first=%+v second=%+v", first, second)
	}
	if first.Replay || !second.Replay {
		t.Fatalf("replay flags: first=%t second=%t", first.Replay, second.Replay)
	}
}

func requireDeterministicProposal(
	t *testing.T,
	first, second *reasoningv1.ImplementationProposal,
	request *reasoningv1.ImplementationRequest,
) {
	t.Helper()
	if !proto.Equal(first, second) {
		t.Fatal("replayed provider output is not deterministic")
	}
	identity := first.GetIdentity()
	envelope := request.GetEnvelope()
	if identity.GetRequestId() != envelope.GetRequestId() ||
		identity.GetTaskId() != envelope.GetTaskId() ||
		identity.GetAttempt() != envelope.GetAttempt() ||
		identity.GetAgentManifestDigest() != envelope.GetAgentManifestDigest() {
		t.Fatal("provider proposal identities were not bound to the request")
	}
}

func requireMiniMaxInvocationMetadata(
	t *testing.T, outcome Outcome, resolver *fakeResolver, adapter *countingAdapter,
) {
	t.Helper()
	if outcome.Invocation.Provider != MiniMaxAnthropicProvider ||
		outcome.Invocation.Model != MiniMaxModel ||
		outcome.Invocation.Usage.ProviderRequests != 1 ||
		resolver.calls.Load() != 1 || adapter.calls.Load() != 1 {
		t.Fatalf(
			"metadata or calls differ: invocation=%+v resolver=%d adapter=%d",
			outcome.Invocation, resolver.calls.Load(), adapter.calls.Load(),
		)
	}
	if outcome.RequestArtifact == (ArtifactReference{}) ||
		outcome.ProposalArtifact == (ArtifactReference{}) {
		t.Fatal("content-addressed artifact references were not returned")
	}
}

func TestMemoryInvocationRepositoryConflictAndRejectedReplay(t *testing.T) {
	t.Run("conflicting request bytes", func(t *testing.T) {
		service, _, adapter := gatewayService(t, gatewayProposal(t))
		request := gatewayRequest(t)
		if _, err := service.ProposeImplementation(t.Context(), request); err != nil {
			t.Fatal(err)
		}
		conflict := proto.Clone(request).(*reasoningv1.ImplementationRequest)
		replacement := "f"
		if conflict.GetBaseCommit()[0] == 'f' {
			replacement = "e"
		}
		conflict.BaseCommit = replacement + conflict.GetBaseCommit()[1:]
		if _, err := service.ProposeImplementation(
			t.Context(), conflict,
		); !errors.Is(err, ErrInvocationConflict) {
			t.Fatalf("conflicting request error = %v", err)
		}
		if adapter.calls.Load() != 1 {
			t.Fatalf("adapter calls = %d; want 1", adapter.calls.Load())
		}
	})

	t.Run("rejected proposal", func(t *testing.T) {
		proposal := gatewayProposal(t)
		proposal.Summary = ""
		service, _, adapter := gatewayService(t, proposal)
		request := gatewayRequest(t)
		first, err := service.ProposeImplementation(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := service.ProposeImplementation(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if first.Replay || !replay.Replay ||
			first.Rejection.GetCode() !=
				reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID ||
			!proto.Equal(first.Rejection, replay.Rejection) {
			t.Fatalf("first=%+v replay=%+v", first, replay)
		}
		if adapter.calls.Load() != 1 {
			t.Fatalf("adapter calls = %d; want 1", adapter.calls.Load())
		}
	})
}

func TestPolicyFailuresReturnStructuredRejections(t *testing.T) {
	t.Run("invalid request authority", func(t *testing.T) {
		service, resolver, adapter := gatewayService(t, gatewayProposal(t))
		request := gatewayRequest(t)
		request.Envelope.Authority.MayApproveWork = true
		outcome, err := service.ProposeImplementation(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Proposal != nil || outcome.Rejection.GetCode() !=
			reasoningv1.RejectionCode_REJECTION_CODE_AUTHORITY_VIOLATION {
			t.Fatalf("outcome = %+v", outcome)
		}
		if resolver.calls.Load() != 0 || adapter.calls.Load() != 0 {
			t.Fatal("invalid request reached infrastructure or adapter")
		}
	})
	t.Run("invalid proposal schema", func(t *testing.T) {
		template := gatewayProposal(t)
		template.Summary = ""
		service, _, adapter := gatewayService(t, template)
		outcome, err := service.ProposeImplementation(t.Context(), gatewayRequest(t))
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Proposal != nil || outcome.Rejection.GetCode() !=
			reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID {
			t.Fatalf("outcome = %+v", outcome)
		}
		if len(outcome.Rejection.GetDetails()) != 1 || adapter.calls.Load() != 1 {
			t.Fatalf(
				"rejection details=%v adapter calls=%d",
				outcome.Rejection, adapter.calls.Load(),
			)
		}
	})
}

func TestInfrastructureCancellationAndAdapterFailuresRemainErrors(t *testing.T) {
	t.Run("resolver", func(t *testing.T) {
		service, resolver, _ := gatewayService(t, gatewayProposal(t))
		resolver.err = errors.New("registry unavailable")
		if _, err := service.ProposeImplementation(
			t.Context(), gatewayRequest(t),
		); err == nil {
			t.Fatal("resolver failure became a policy outcome")
		}
	})
	t.Run("adapter", func(t *testing.T) {
		service, _, adapter := gatewayService(t, gatewayProposal(t))
		adapter.err = errors.New("adapter unavailable")
		if _, err := service.ProposeImplementation(
			t.Context(), gatewayRequest(t),
		); err == nil {
			t.Fatal("adapter failure became a policy outcome")
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		service, resolver, adapter := gatewayService(t, gatewayProposal(t))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := service.ProposeImplementation(
			ctx, gatewayRequest(t),
		); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
		if resolver.calls.Load() != 0 || adapter.calls.Load() != 0 {
			t.Fatal("cancelled request reached dependencies")
		}
	})
}

func TestManifestMustResolveToExactImplementationPairing(t *testing.T) {
	service, resolver, adapter := gatewayService(t, gatewayProposal(t))
	resolver.resolved.Manifest.Stage = "review"
	if _, err := service.ProposeImplementation(
		t.Context(), gatewayRequest(t),
	); err == nil {
		t.Fatal("wrong manifest stage accepted")
	}
	if adapter.calls.Load() != 0 {
		t.Fatal("wrong manifest pairing reached adapter")
	}
}

func TestGatewayDefaultByteLimits(t *testing.T) {
	limits := DefaultByteLimits()
	if limits.Request != 1<<20 || limits.Proposal != 1<<20 ||
		limits.ProviderResponse != 1<<20 {
		t.Fatalf("default limits = %+v", limits)
	}
}

func TestGatewayRequestByteLimit(t *testing.T) {
	service, resolver, adapter := gatewayService(t, gatewayProposal(t))
	service.limits.Request = 1
	outcome, err := service.ProposeImplementation(t.Context(), gatewayRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Rejection.GetCode() !=
		reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID {
		t.Fatalf("outcome = %+v", outcome)
	}
	if resolver.calls.Load() != 0 || adapter.calls.Load() != 0 {
		t.Fatal("oversized request reached dependencies")
	}
}

func TestGatewayProposalByteLimit(t *testing.T) {
	service, _, adapter := gatewayService(t, gatewayProposal(t))
	service.limits.Proposal = 1
	outcome, err := service.ProposeImplementation(t.Context(), gatewayRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Rejection.GetCode() !=
		reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID ||
		outcome.ProposalArtifact != (ArtifactReference{}) ||
		adapter.calls.Load() != 1 {
		t.Fatalf("outcome=%+v adapter calls=%d", outcome, adapter.calls.Load())
	}
}

func TestGatewayProviderResponseByteLimit(t *testing.T) {
	service, _, adapter := gatewayService(t, gatewayProposal(t))
	request := gatewayRequest(t)
	service.limits.ProviderResponse = 1
	if _, err := service.ProposeImplementation(t.Context(), request); err == nil {
		t.Fatal("oversized provider response accepted")
	}
	service.limits.ProviderResponse = defaultMaximumBytes
	outcome, err := service.ProposeImplementation(t.Context(), request)
	if err != nil || outcome.Proposal == nil || adapter.calls.Load() != 2 {
		t.Fatalf("retry outcome=%+v calls=%d err=%v", outcome, adapter.calls.Load(), err)
	}
}

func TestGatewayAllowsMultipleProviderRequestsWithinBudget(t *testing.T) {
	service, _, adapter := gatewayService(t, gatewayProposal(t))
	request := gatewayRequest(t)
	request.Envelope.Budget.MaximumProviderRequests = 2
	original := adapter.inner
	adapter.inner = implementationAdapterFunc(func(
		ctx context.Context, value manifest.Manifest,
		request *reasoningv1.ImplementationRequest,
	) (AdapterResult, error) {
		result, err := original.ProposeImplementation(ctx, value, request)
		result.Usage.ProviderRequests = 2
		return result, err
	})
	outcome, err := service.ProposeImplementation(t.Context(), request)
	if err != nil || outcome.Proposal == nil ||
		outcome.Invocation.Usage.ProviderRequests != 2 {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
}

func TestMalformedProviderOutputIsPersistedAndReplayed(t *testing.T) {
	service, resolver, adapter := gatewayService(t, gatewayProposal(t))
	response := []byte(`{"unexpected":true}`)
	adapter.inner = implementationAdapterFunc(func(
		context.Context, manifest.Manifest, *reasoningv1.ImplementationRequest,
	) (AdapterResult, error) {
		return AdapterResult{
			ProviderResponse: response,
			MalformedOutput:  &MalformedOutput{Message: "unknown field unexpected"},
			Provider:         "provider", Model: "model",
			ProviderRequestID: "req_123", Usage: Usage{ProviderRequests: 1},
		}, nil
	})
	request := gatewayRequest(t)
	first, err := service.ProposeImplementation(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ProposeImplementation(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Rejection.GetCode() != reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID ||
		first.ProviderResponseArtifact == (ArtifactReference{}) ||
		first.Invocation.ProviderRequestID != "req_123" || !second.Replay ||
		adapter.calls.Load() != 1 || resolver.calls.Load() != 1 {
		t.Fatalf(
			"first=%+v second=%+v adapter=%d resolver=%d",
			first, second, adapter.calls.Load(), resolver.calls.Load(),
		)
	}
	store := service.artifacts.(*memoryArtifactStore)
	stored, err := store.Get(t.Context(), first.ProviderResponseArtifact)
	if err != nil || !bytes.Equal(stored, response) {
		t.Fatalf("stored=%q err=%v", stored, err)
	}
}

func TestEmptyChangeProposalIsSchemaInvalidAndCannotReachExecution(t *testing.T) {
	proposal := gatewayProposal(t)
	proposal.Changes = nil
	proposal.ScopeChangeRequest = nil
	service, _, adapter := gatewayService(t, proposal)

	outcome, err := service.ProposeImplementation(t.Context(), gatewayRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Proposal != nil ||
		outcome.Rejection.GetCode() !=
			reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID ||
		outcome.ProposalArtifact == (ArtifactReference{}) || adapter.calls.Load() != 1 {
		t.Fatalf("outcome=%+v adapter calls=%d", outcome, adapter.calls.Load())
	}
}

func TestGatewayArtifactFailuresAndReplayIntegrity(t *testing.T) {
	t.Run("request put", func(t *testing.T) {
		service, _, adapter := gatewayService(t, gatewayProposal(t))
		store := service.artifacts.(*memoryArtifactStore)
		store.failPut = 1
		if _, err := service.ProposeImplementation(
			t.Context(), gatewayRequest(t),
		); err == nil {
			t.Fatal("request artifact failure became a policy outcome")
		}
		if adapter.calls.Load() != 0 {
			t.Fatal("request artifact failure reached adapter")
		}
	})
	t.Run("proposal put rolls back", func(t *testing.T) {
		service, _, adapter := gatewayService(t, gatewayProposal(t))
		store := service.artifacts.(*memoryArtifactStore)
		store.failPut = 2
		request := gatewayRequest(t)
		if _, err := service.ProposeImplementation(t.Context(), request); err == nil {
			t.Fatal("proposal artifact failure became a policy outcome")
		}
		store.failPut = 0
		outcome, err := service.ProposeImplementation(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Replay || adapter.calls.Load() != 2 {
			t.Fatalf("rollback replay=%t adapter calls=%d", outcome.Replay, adapter.calls.Load())
		}
	})
	for _, test := range []struct {
		name   string
		mutate func(*memoryArtifactStore, ArtifactReference)
	}{
		{
			name: "missing",
			mutate: func(store *memoryArtifactStore, reference ArtifactReference) {
				delete(store.values, reference.SHA256)
			},
		},
		{
			name: "corrupt",
			mutate: func(store *memoryArtifactStore, reference ArtifactReference) {
				store.values[reference.SHA256] = []byte("corrupt")
			},
		},
	} {
		t.Run("replay "+test.name, func(t *testing.T) {
			service, _, adapter := gatewayService(t, gatewayProposal(t))
			request := gatewayRequest(t)
			first, err := service.ProposeImplementation(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			store := service.artifacts.(*memoryArtifactStore)
			store.mu.Lock()
			test.mutate(store, first.ProposalArtifact)
			store.mu.Unlock()
			if _, err := service.ProposeImplementation(
				t.Context(), request,
			); err == nil {
				t.Fatal("invalid replay artifact accepted")
			}
			if adapter.calls.Load() != 1 {
				t.Fatal("invalid replay invoked adapter")
			}
		})
	}
	t.Run("replay corrupt provider response", func(t *testing.T) {
		service, _, adapter := gatewayService(t, gatewayProposal(t))
		request := gatewayRequest(t)
		first, err := service.ProposeImplementation(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		store := service.artifacts.(*memoryArtifactStore)
		store.mu.Lock()
		store.values[first.ProviderResponseArtifact.SHA256] = []byte("corrupt")
		store.mu.Unlock()
		if _, err := service.ProposeImplementation(t.Context(), request); err == nil {
			t.Fatal("corrupt provider response replay artifact accepted")
		}
		if adapter.calls.Load() != 1 {
			t.Fatal("corrupt provider response replay invoked adapter")
		}
	})
}

func TestGatewayConcurrentIdenticalRequestsReplay(t *testing.T) {
	service, _, adapter := gatewayService(t, gatewayProposal(t))
	const calls = 12
	outcomes := make(chan Outcome, calls)
	errs := make(chan error, calls)
	request := gatewayRequest(t)
	for range calls {
		go func() {
			outcome, err := service.ProposeImplementation(
				context.Background(),
				proto.Clone(request).(*reasoningv1.ImplementationRequest),
			)
			outcomes <- outcome
			errs <- err
		}()
	}
	replays := 0
	for range calls {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if (<-outcomes).Replay {
			replays++
		}
	}
	if adapter.calls.Load() != 1 || replays != calls-1 {
		t.Fatalf("adapter calls=%d replays=%d", adapter.calls.Load(), replays)
	}
}

func TestGatewayConcurrentConflictingRequests(t *testing.T) {
	service, _, adapter := gatewayService(t, gatewayProposal(t))
	first := gatewayRequest(t)
	second := proto.Clone(first).(*reasoningv1.ImplementationRequest)
	replacement := "f"
	if second.GetBaseCommit()[0] == 'f' {
		replacement = "e"
	}
	second.BaseCommit = replacement + second.GetBaseCommit()[1:]
	errs := make(chan error, 2)
	for _, request := range []*reasoningv1.ImplementationRequest{first, second} {
		go func() {
			_, err := service.ProposeImplementation(context.Background(), request)
			errs <- err
		}()
	}
	var successes, conflicts int
	for range 2 {
		err := <-errs
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrInvocationConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 || adapter.calls.Load() != 1 {
		t.Fatalf(
			"successes=%d conflicts=%d adapter calls=%d",
			successes, conflicts, adapter.calls.Load(),
		)
	}
}

func TestExpiredRequestIsRejectedBeforeAdapter(t *testing.T) {
	service, _, adapter := gatewayService(t, gatewayProposal(t))
	request := gatewayRequest(t)
	service.clock = fixedClock{
		now: request.GetEnvelope().GetExpiresAt().AsTime().Add(time.Nanosecond),
	}
	outcome, err := service.ProposeImplementation(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Rejection.GetCode() !=
		reasoningv1.RejectionCode_REJECTION_CODE_REQUEST_MISMATCH ||
		adapter.calls.Load() != 0 {
		t.Fatalf("outcome=%+v adapter calls=%d", outcome, adapter.calls.Load())
	}
}
