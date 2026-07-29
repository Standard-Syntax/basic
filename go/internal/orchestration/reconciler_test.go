package orchestration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

type memoryLedger struct {
	job       runtime.Job
	found     bool
	completed int
	retried   int
	failed    int
	enqueued  []runtime.Job
}

func (m *memoryLedger) Claim(context.Context, string, time.Time, time.Duration) (runtime.Job, bool, error) {
	if !m.found {
		return runtime.Job{}, false, nil
	}
	m.found = false
	return m.job, true, nil
}
func (m *memoryLedger) Complete(context.Context, string, string, uint64, workflow.ArtifactRef, time.Time) error {
	m.completed++
	return nil
}
func (m *memoryLedger) Retry(context.Context, string, string, uint64, time.Time) error {
	m.retried++
	return nil
}
func (m *memoryLedger) Fail(context.Context, string, string, uint64, workflow.ArtifactRef, time.Time) error {
	m.failed++
	return nil
}
func (m *memoryLedger) Enqueue(_ context.Context, job runtime.Job) error {
	m.enqueued = append(m.enqueued, job)
	return nil
}

type memoryArtifacts struct{}

func (memoryArtifacts) Put(_ context.Context, body []byte) (workflow.ArtifactRef, error) {
	digest := runtime.Digest(body)
	return workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}, nil
}

func handlersReturning(err error) map[string]Handler {
	handlers := make(map[string]Handler)
	for _, stage := range orderedStages {
		handlers[stage] = HandlerFunc(func(
			_ context.Context, _ runtime.Job, ids Identities,
		) (workflow.ArtifactRef, error) {
			if err != nil {
				return workflow.ArtifactRef{}, err
			}
			digest := runtime.Digest([]byte(ids.ActivityID))
			return workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}, nil
		})
	}
	return handlers
}

func TestOnceCompletesAndEnqueuesStableNextStage(t *testing.T) {
	runID, taskID, owner := uuid.NewString(), uuid.NewString(), uuid.NewString()
	ledger := &memoryLedger{found: true, job: runtime.Job{
		ID: uuid.NewString(), RunID: runID, TaskID: &taskID, Attempt: 1,
		Stage: StageStart, FencingToken: 2,
	}}
	reconciler, err := New(Config{
		OwnerID: owner, ClaimTTL: time.Minute, PollInterval: time.Millisecond,
		MaxRetries: 3, InitialBackoff: time.Second,
	}, ledger, memoryArtifacts{}, handlersReturning(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := reconciler.Once(context.Background()); err != nil || !worked {
		t.Fatalf("once = %v, %v", worked, err)
	}
	if ledger.completed != 1 || len(ledger.enqueued) != 1 ||
		ledger.enqueued[0].Stage != StageImplementationRequest {
		t.Fatalf("ledger = %#v", ledger)
	}
	expected := StableID(runID, taskID, "1", StageImplementationRequest, "job")
	if ledger.enqueued[0].ID != expected {
		t.Fatalf("job id = %s, want %s", ledger.enqueued[0].ID, expected)
	}
}

func TestOnceRetriesThenPersistsFailureEvidence(t *testing.T) {
	for _, test := range []struct {
		retries uint32
		retried int
		failed  int
	}{{0, 1, 0}, {2, 0, 1}} {
		ledger := &memoryLedger{found: true, job: runtime.Job{
			ID: uuid.NewString(), RunID: uuid.NewString(), Attempt: 1,
			Stage: StageReasoning, FencingToken: 1, RetryCount: test.retries,
		}}
		reconciler, err := New(Config{
			OwnerID: uuid.NewString(), ClaimTTL: time.Minute,
			PollInterval: time.Millisecond, MaxRetries: 3, InitialBackoff: time.Second,
		}, ledger, memoryArtifacts{}, handlersReturning(errors.New("injected")), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reconciler.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
		if ledger.retried != test.retried || ledger.failed != test.failed {
			t.Fatalf("retry=%d ledger=%#v", test.retries, ledger)
		}
	}
}
