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
	completed []completeCall
	retried   []retryCall
	failed    []failureCall
}

type completeCall struct {
	jobID, owner string
	fence        uint64
	result       workflow.ArtifactRef
	next         *runtime.Job
	at           time.Time
}

type retryCall struct {
	jobID, owner string
	fence        uint64
	available    time.Time
}

type failureCall struct {
	jobID, owner string
	fence        uint64
	failure      workflow.ArtifactRef
	at           time.Time
}

func (m *memoryLedger) Claim(context.Context, string, time.Time, time.Duration) (runtime.Job, bool, error) {
	if !m.found {
		return runtime.Job{}, false, nil
	}
	m.found = false
	return m.job, true, nil
}
func (m *memoryLedger) CompleteAndEnqueue(
	_ context.Context, jobID, owner string, fence uint64,
	result workflow.ArtifactRef, next *runtime.Job, at time.Time,
) error {
	var clonedNext *runtime.Job
	if next != nil {
		value := *next
		clonedNext = &value
	}
	m.completed = append(m.completed, completeCall{
		jobID: jobID, owner: owner, fence: fence, result: result, next: clonedNext, at: at,
	})
	return nil
}
func (m *memoryLedger) Retry(
	_ context.Context, jobID, owner string, fence uint64, available time.Time,
) error {
	m.retried = append(m.retried, retryCall{
		jobID: jobID, owner: owner, fence: fence, available: available,
	})
	return nil
}
func (m *memoryLedger) Fail(
	_ context.Context, jobID, owner string, fence uint64,
	failure workflow.ArtifactRef, at time.Time,
) error {
	m.failed = append(m.failed, failureCall{
		jobID: jobID, owner: owner, fence: fence, failure: failure, at: at,
	})
	return nil
}

type memoryArtifacts struct{ bodies [][]byte }

func (m *memoryArtifacts) Put(_ context.Context, body []byte) (workflow.ArtifactRef, error) {
	m.bodies = append(m.bodies, append([]byte(nil), body...))
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
	}, ledger, &memoryArtifacts{}, handlersReturning(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := reconciler.Once(context.Background()); err != nil || !worked {
		t.Fatalf("once = %v, %v", worked, err)
	}
	if len(ledger.completed) != 1 || ledger.completed[0].next == nil ||
		ledger.completed[0].next.Stage != StageImplementationRequest {
		t.Fatalf("ledger = %#v", ledger)
	}
	call := ledger.completed[0]
	if call.jobID != ledger.job.ID || call.owner != owner ||
		call.fence != ledger.job.FencingToken || call.result.Digest == "" {
		t.Fatalf("completion = %#v", call)
	}
	expected := StableID(runID, taskID, "1", StageImplementationRequest, "job")
	if call.next.ID != expected {
		t.Fatalf("job id = %s, want %s", call.next.ID, expected)
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
		artifacts := &memoryArtifacts{}
		fixedNow := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
		owner := uuid.NewString()
		reconciler, err := New(Config{
			OwnerID: owner, ClaimTTL: time.Minute,
			PollInterval: time.Millisecond, MaxRetries: 3, InitialBackoff: time.Second,
		}, ledger, artifacts, handlersReturning(errors.New("injected")), nil)
		if err != nil {
			t.Fatal(err)
		}
		reconciler.now = func() time.Time { return fixedNow }
		if _, err := reconciler.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(ledger.retried) != test.retried || len(ledger.failed) != test.failed {
			t.Fatalf("retry=%d ledger=%#v", test.retries, ledger)
		}
		if test.retried == 1 {
			call := ledger.retried[0]
			if call.jobID != ledger.job.ID || call.owner != owner ||
				call.fence != ledger.job.FencingToken ||
				!call.available.Equal(fixedNow.Add(time.Second)) {
				t.Fatalf("retry call = %#v", call)
			}
		}
		if test.failed == 1 {
			if len(artifacts.bodies) != 1 {
				t.Fatalf("failure evidence bodies = %d", len(artifacts.bodies))
			}
			call := ledger.failed[0]
			if call.jobID != ledger.job.ID || call.owner != owner ||
				call.fence != ledger.job.FencingToken ||
				call.failure.Digest != runtime.Digest(artifacts.bodies[0]) ||
				!call.at.Equal(fixedNow) {
				t.Fatalf("failure call = %#v", call)
			}
		}
	}
}
