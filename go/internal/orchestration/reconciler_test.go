package orchestration

import (
	"context"
	"errors"
	"testing"
	"time"

	postgresutil "github.com/Standard-Syntax/basic/go/internal/postgres"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

type memoryLedger struct {
	job         runtime.Job
	found       bool
	completed   []completeCall
	retried     []retryCall
	rescheduled []retryCall
	failed      []failureCall
	renewals    int
	renew       func(context.Context) error
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
func (m *memoryLedger) Renew(
	ctx context.Context, _ string, _ string, _ uint64, _ time.Time,
) error {
	m.renewals++
	if m.renew != nil {
		return m.renew(ctx)
	}
	return nil
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
func (m *memoryLedger) RescheduleTransient(
	_ context.Context, jobID, owner string, fence uint64, available time.Time,
) error {
	m.rescheduled = append(m.rescheduled, retryCall{
		jobID: jobID, owner: owner, fence: fence, available: available,
	})
	return nil
}

func TestOnceReschedulesTransientDatabaseConflictWithoutRetryCharge(t *testing.T) {
	owner, fixedNow := uuid.NewString(), time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	ledger := &memoryLedger{found: true, job: runtime.Job{
		ID: uuid.NewString(), RunID: uuid.NewString(), Stage: StageReasoning,
		FencingToken: 7, RetryCount: 2,
	}}
	reconciler, err := New(Config{OwnerID: owner, ClaimTTL: time.Minute,
		PollInterval: time.Millisecond, MaxRetries: 3, InitialBackoff: 10 * time.Millisecond},
		ledger, &memoryArtifacts{}, handlersReturning(postgresutil.ErrTransient), nil)
	if err != nil {
		t.Fatal(err)
	}
	reconciler.now = func() time.Time { return fixedNow }
	worked, err := reconciler.Once(t.Context())
	if err != nil || !worked || len(ledger.rescheduled) != 1 || len(ledger.retried) != 0 || len(ledger.failed) != 0 {
		t.Fatalf("worked=%v err=%v rescheduled=%v retried=%v failed=%v",
			worked, err, ledger.rescheduled, ledger.retried, ledger.failed)
	}
}

func TestOnceFailsAfterBoundedTransientDatabaseConflicts(t *testing.T) {
	owner := uuid.NewString()
	ledger := &memoryLedger{found: true, job: runtime.Job{
		ID: uuid.NewString(), RunID: uuid.NewString(), Stage: StageReasoning,
		FencingToken: 7, RetryCount: 0, TransientRescheduleCount: 2,
	}}
	reconciler, err := New(Config{OwnerID: owner, ClaimTTL: time.Minute,
		PollInterval: time.Millisecond, MaxRetries: 3, InitialBackoff: 10 * time.Millisecond},
		ledger, &memoryArtifacts{}, handlersReturning(postgresutil.ErrTransient), nil)
	if err != nil {
		t.Fatal(err)
	}
	worked, err := reconciler.Once(t.Context())
	if err != nil || !worked || len(ledger.rescheduled) != 0 || len(ledger.failed) != 1 ||
		ledger.job.RetryCount != 0 {
		t.Fatalf("worked=%v err=%v rescheduled=%v failed=%v job=%+v",
			worked, err, ledger.rescheduled, ledger.failed, ledger.job)
	}
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
		) (HandlerResult, error) {
			if err != nil {
				return HandlerResult{}, err
			}
			digest := runtime.Digest([]byte(ids.ActivityID))
			return HandlerResult{Artifact: workflow.ArtifactRef{
				URI: "artifact://sha256/" + digest, Digest: digest,
			}, Continue: true}, nil
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

func TestFailedVerificationStopsBeforeReviewAndRenewsLongClaim(t *testing.T) {
	runID, taskID, owner := uuid.NewString(), uuid.NewString(), uuid.NewString()
	ledger := &memoryLedger{found: true, job: runtime.Job{
		ID: uuid.NewString(), RunID: runID, TaskID: &taskID, Attempt: 1,
		Stage: StageVerification, FencingToken: 4,
	}}
	handlers := handlersReturning(nil)
	handlers[StageVerification] = HandlerFunc(func(
		ctx context.Context, _ runtime.Job, _ Identities,
	) (HandlerResult, error) {
		select {
		case <-ctx.Done():
			return HandlerResult{}, ctx.Err()
		case <-time.After(15 * time.Millisecond):
		}
		digest := runtime.Digest([]byte("failed-verification"))
		return HandlerResult{Artifact: workflow.ArtifactRef{
			URI: "artifact://sha256/" + digest, Digest: digest,
		}, Continue: false}, nil
	})
	reconciler, err := New(Config{
		OwnerID: owner, ClaimTTL: 30 * time.Millisecond,
		PollInterval: time.Millisecond, MaxRetries: 3, InitialBackoff: time.Millisecond,
	}, ledger, &memoryArtifacts{}, handlers, nil)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := reconciler.Once(t.Context()); err != nil || !worked {
		t.Fatalf("once = %v, %v", worked, err)
	}
	if ledger.renewals == 0 || len(ledger.completed) != 1 ||
		ledger.completed[0].next != nil {
		t.Fatalf("renewals=%d completed=%#v", ledger.renewals, ledger.completed)
	}
}

func TestRunWithDrainRenewsAndCompletesActiveClaim(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ledger := &memoryLedger{found: true, job: runtime.Job{
		ID: uuid.NewString(), RunID: uuid.NewString(), Stage: StageStart, FencingToken: 1,
	}}
	handlers := handlersReturning(nil)
	handlers[StageStart] = HandlerFunc(func(
		ctx context.Context, _ runtime.Job, _ Identities,
	) (HandlerResult, error) {
		close(started)
		select {
		case <-release:
			return HandlerResult{Artifact: workflow.ArtifactRef{URI: "artifact://done", Digest: "done"}}, nil
		case <-ctx.Done():
			return HandlerResult{}, ctx.Err()
		}
	})
	reconciler, err := New(Config{OwnerID: uuid.NewString(), ClaimTTL: 9 * time.Millisecond,
		PollInterval: time.Millisecond, MaxRetries: 3, InitialBackoff: time.Millisecond,
	}, ledger, &memoryArtifacts{}, handlers, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- reconciler.RunWithDrain(ctx, time.Second) }()
	<-started
	cancel()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("drain = %v", err)
	}
	if ledger.renewals == 0 || len(ledger.completed) != 1 {
		t.Fatalf("renewals=%d completed=%d", ledger.renewals, len(ledger.completed))
	}
}

func TestRunWithDrainCancelsAtDeadline(t *testing.T) {
	started := make(chan struct{})
	ledger := &memoryLedger{found: true, job: runtime.Job{
		ID: uuid.NewString(), RunID: uuid.NewString(), Stage: StageStart, FencingToken: 1,
	}}
	handlers := handlersReturning(nil)
	handlers[StageStart] = HandlerFunc(func(ctx context.Context, _ runtime.Job, _ Identities) (HandlerResult, error) {
		close(started)
		<-ctx.Done()
		return HandlerResult{}, ctx.Err()
	})
	reconciler, err := New(Config{OwnerID: uuid.NewString(), ClaimTTL: time.Second,
		PollInterval: time.Millisecond, MaxRetries: 3, InitialBackoff: time.Millisecond,
	}, ledger, &memoryArtifacts{}, handlers, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- reconciler.RunWithDrain(ctx, 10*time.Millisecond) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain = %v", err)
	}
	if len(ledger.completed) != 0 {
		t.Fatal("expired drain completed stale claim")
	}
}

func TestOnceIgnoresRenewalCancellationAfterHandlerCompletes(t *testing.T) {
	runID, taskID, owner := uuid.NewString(), uuid.NewString(), uuid.NewString()
	renewStarted := make(chan struct{})
	ledger := &memoryLedger{found: true, job: runtime.Job{
		ID: uuid.NewString(), RunID: runID, TaskID: &taskID, Attempt: 1,
		Stage: StageVerification, FencingToken: 4,
	}}
	ledger.renew = func(ctx context.Context) error {
		close(renewStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	handlers := handlersReturning(nil)
	handlers[StageVerification] = HandlerFunc(func(
		_ context.Context, _ runtime.Job, ids Identities,
	) (HandlerResult, error) {
		<-renewStarted
		digest := runtime.Digest([]byte(ids.ActivityID))
		return HandlerResult{Artifact: workflow.ArtifactRef{
			URI: "artifact://sha256/" + digest, Digest: digest,
		}, Continue: true}, nil
	})
	reconciler, err := New(Config{
		OwnerID: owner, ClaimTTL: 3 * time.Millisecond,
		PollInterval: time.Millisecond, MaxRetries: 3, InitialBackoff: time.Millisecond,
	}, ledger, &memoryArtifacts{}, handlers, nil)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := reconciler.Once(t.Context()); err != nil || !worked {
		t.Fatalf("once = %v, %v", worked, err)
	}
	if len(ledger.completed) != 1 || len(ledger.retried) != 0 {
		t.Fatalf("completed=%#v retried=%#v", ledger.completed, ledger.retried)
	}
}

func TestOnceRetriesThenPersistsFailureEvidence(t *testing.T) {
	ledger, _, owner, fixedNow := runFailureCase(t, 0)
	if len(ledger.retried) != 1 || len(ledger.failed) != 0 {
		t.Fatalf("ledger = %#v", ledger)
	}
	call := ledger.retried[0]
	if call.jobID != ledger.job.ID || call.owner != owner ||
		call.fence != ledger.job.FencingToken ||
		!call.available.Equal(fixedNow.Add(time.Second)) {
		t.Fatalf("retry call = %#v", call)
	}
}

func TestOncePersistsFailureEvidenceAfterRetryExhaustion(t *testing.T) {
	ledger, artifacts, owner, fixedNow := runFailureCase(t, 2)
	if len(ledger.retried) != 0 || len(ledger.failed) != 1 ||
		len(ledger.completed) != 0 || len(artifacts.bodies) != 1 {
		t.Fatalf("ledger=%#v bodies=%d", ledger, len(artifacts.bodies))
	}
	call := ledger.failed[0]
	if call.jobID != ledger.job.ID || call.owner != owner ||
		call.fence != ledger.job.FencingToken ||
		call.failure.Digest != runtime.Digest(artifacts.bodies[0]) ||
		!call.at.Equal(fixedNow) {
		t.Fatalf("failure call = %#v", call)
	}
}

func runFailureCase(
	t *testing.T, retries uint32,
) (*memoryLedger, *memoryArtifacts, string, time.Time) {
	t.Helper()
	ledger := &memoryLedger{found: true, job: runtime.Job{
		ID: uuid.NewString(), RunID: uuid.NewString(), Attempt: 1,
		Stage: StageReasoning, FencingToken: 1, RetryCount: retries,
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
	return ledger, artifacts, owner, fixedNow
}
