package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
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
	calls    int
}

func (r *fakeResolver) ResolveManifest(
	_ context.Context, _ string,
) (ResolvedManifest, error) {
	r.calls++
	return r.resolved, r.err
}

type countingAdapter struct {
	inner ImplementationAdapter
	err   error
	calls int
}

type memoryArtifactStore struct {
	mu     sync.Mutex
	values map[string][]byte
}

func newMemoryArtifactStore() *memoryArtifactStore {
	return &memoryArtifactStore{values: make(map[string][]byte)}
}

func (s *memoryArtifactStore) Put(
	_ context.Context, body []byte,
) (ArtifactReference, error) {
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
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
	a.calls++
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
		Digest:   request.GetEnvelope().GetAgentManifestDigest(),
		Manifest: implementationManifest(t),
	}}
	fake, err := NewFakeImplementationAdapter(
		template, "fake-implementation-v1",
		Usage{InputTokens: 11, OutputTokens: 7, ProviderRequests: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &countingAdapter{inner: fake}
	service, err := NewService(
		resolver, adapter, newMemoryArtifactStore(), newMemoryInvocationRepository(),
		fixedClock{now: request.GetEnvelope().GetCreatedAt().AsTime().Add(time.Minute)},
	)
	if err != nil {
		t.Fatal(err)
	}
	return service, resolver, adapter
}

func TestFakeImplementationReturnsOneDeterministicValidatedProposal(t *testing.T) {
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
	requireFakeInvocationMetadata(t, first, resolver, adapter)
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
		t.Fatal("fake adapter output is not deterministic")
	}
	identity := first.GetIdentity()
	envelope := request.GetEnvelope()
	if identity.GetRequestId() != envelope.GetRequestId() ||
		identity.GetTaskId() != envelope.GetTaskId() ||
		identity.GetAttempt() != envelope.GetAttempt() ||
		identity.GetAgentManifestDigest() != envelope.GetAgentManifestDigest() {
		t.Fatal("fake proposal identities were not bound to the request")
	}
}

func requireFakeInvocationMetadata(
	t *testing.T, outcome Outcome, resolver *fakeResolver, adapter *countingAdapter,
) {
	t.Helper()
	if outcome.Invocation.Provider != FakeProvider ||
		outcome.Invocation.Usage.ProviderRequests != 1 ||
		resolver.calls != 1 || adapter.calls != 1 {
		t.Fatalf(
			"metadata or calls differ: invocation=%+v resolver=%d adapter=%d",
			outcome.Invocation, resolver.calls, adapter.calls,
		)
	}
	if outcome.RequestArtifact == (ArtifactReference{}) ||
		outcome.ProposalArtifact == (ArtifactReference{}) {
		t.Fatal("content-addressed artifact references were not returned")
	}
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
		if resolver.calls != 0 || adapter.calls != 0 {
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
		if len(outcome.Rejection.GetDetails()) != 1 || adapter.calls != 1 {
			t.Fatalf("rejection details=%v adapter calls=%d", outcome.Rejection, adapter.calls)
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
		if resolver.calls != 0 || adapter.calls != 0 {
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
	if adapter.calls != 0 {
		t.Fatal("wrong manifest pairing reached adapter")
	}
}
