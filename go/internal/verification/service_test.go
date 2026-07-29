package verification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type memoryArtifacts struct {
	mu     sync.Mutex
	values map[string][]byte
}

func (m *memoryArtifacts) Get(
	_ context.Context, artifact workflow.ArtifactRef,
) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	body, ok := m.values[artifact.Digest]
	if !ok {
		return nil, errors.New("artifact not found")
	}
	return append([]byte(nil), body...), nil
}

func (m *memoryArtifacts) Put(
	_ context.Context, body []byte,
) (workflow.ArtifactRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	m.values[digest] = append([]byte(nil), body...)
	return workflow.ArtifactRef{
		URI: "artifact://sha256/" + digest, Digest: digest,
	}, nil
}

type fakeWorkflow struct {
	mu       sync.Mutex
	commands []workflow.RecordTaskVerification
	fail     atomic.Bool
}

func (f *fakeWorkflow) ExecuteTask(
	_ context.Context, command workflow.TaskCommand,
) (workflow.CommandResult, error) {
	if f.fail.Load() {
		return workflow.CommandResult{}, errors.New("workflow unavailable")
	}
	mapped, ok := command.(workflow.RecordTaskVerification)
	if !ok {
		return workflow.CommandResult{}, errors.New("unexpected command")
	}
	f.mu.Lock()
	f.commands = append(f.commands, mapped)
	f.mu.Unlock()
	return workflow.CommandResult{Revision: mapped.Meta.ExpectedRevision + 1}, nil
}

type fakePreparer struct {
	root      string
	prepared  atomic.Int32
	cleaned   atomic.Int32
	candidate string
}

func (p *fakePreparer) Prepare(
	_ context.Context, _, candidate string,
) (string, func() error, error) {
	p.prepared.Add(1)
	p.candidate = candidate
	workspace, err := os.MkdirTemp(p.root, "verify-")
	if err != nil {
		return "", nil, err
	}
	return workspace, func() error {
		p.cleaned.Add(1)
		return os.RemoveAll(workspace)
	}, nil
}

type fakeExecutor struct {
	mu           sync.Mutex
	measurements []ExecutionMeasurement
	calls        []string
	imageID      string
	err          error
}

func (f *fakeExecutor) ImageID(context.Context) (string, error) {
	if f.imageID == "" {
		f.imageID = "sha256:" + string(make([]byte, 64))
	}
	return f.imageID, nil
}

func (f *fakeExecutor) Run(
	_ context.Context, _, imageID string, definition CheckDefinition,
) (ExecutionMeasurement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, definition.ID+"@"+imageID)
	if f.err != nil {
		return ExecutionMeasurement{}, f.err
	}
	index := len(f.calls) - 1
	if index >= len(f.measurements) {
		index = len(f.measurements) - 1
	}
	return f.measurements[index], nil
}

func TestServiceRecordsPassingAndFailingEvidence(t *testing.T) {
	for _, test := range []struct {
		name        string
		measurement ExecutionMeasurement
		passed      bool
	}{
		{name: "passing", measurement: passingMeasurement(), passed: true},
		{name: "failing", measurement: failingMeasurement(), passed: false},
		{name: "timeout", measurement: timeoutMeasurement(), passed: false},
		{name: "output overflow", measurement: overflowMeasurement(), passed: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, request, workflowStore, executor, preparer := serviceFixture(
				t, []ExecutionMeasurement{test.measurement},
			)
			result, err := service.Verify(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if result.Passed != test.passed || len(workflowStore.commands) != 1 ||
				workflowStore.commands[0].Passed != test.passed {
				t.Fatalf("result=%#v commands=%#v", result, workflowStore.commands)
			}
			if len(executor.calls) != 1 || preparer.cleaned.Load() != 1 {
				t.Fatalf("calls=%v cleaned=%d", executor.calls, preparer.cleaned.Load())
			}
		})
	}
}

func TestServiceFailsClosedForUncoveredCriterionWithoutTrustingModelText(t *testing.T) {
	service, request, workflowStore, executor, _ := serviceFixture(
		t, []ExecutionMeasurement{passingMeasurement()},
	)
	request.Requirements[0].CheckIDs = nil
	result, err := service.Verify(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed || workflowStore.commands[0].Passed || len(executor.calls) != 1 {
		t.Fatalf("uncovered criterion passed: %#v calls=%v", result, executor.calls)
	}
}

func TestServiceRejectsArtifactAndCandidateMismatchesBeforeExecution(t *testing.T) {
	cases := map[string]func(*Request, *memoryArtifacts){
		"corrupt artifact": func(request *Request, artifacts *memoryArtifacts) {
			artifacts.values[request.ExecutionArtifact.Digest] = []byte("corrupt")
		},
		"candidate mismatch": func(request *Request, _ *memoryArtifacts) {
			request.CandidateCommit = "d" + request.CandidateCommit[1:]
		},
		"stale revision": func(request *Request, _ *memoryArtifacts) {
			request.ExpectedTaskRevision = 0
		},
		"unknown check": func(request *Request, _ *memoryArtifacts) {
			request.Requirements[0].CheckIDs = []string{"model-shell-v1"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			service, request, workflowStore, executor, preparer := serviceFixture(
				t, []ExecutionMeasurement{passingMeasurement()},
			)
			mutate(&request, service.artifacts.(*memoryArtifacts))
			if _, err := service.Verify(t.Context(), request); err == nil {
				t.Fatal("invalid request accepted")
			}
			if len(executor.calls) != 0 || len(workflowStore.commands) != 0 ||
				preparer.prepared.Load() != 0 {
				t.Fatal("invalid request caused a side effect")
			}
		})
	}
}

func TestServiceReplayAndEvidenceReadyRecoveryDoNotRerunChecks(t *testing.T) {
	service, request, workflowStore, executor, _ := serviceFixture(
		t, []ExecutionMeasurement{passingMeasurement()},
	)
	workflowStore.fail.Store(true)
	if _, err := service.Verify(t.Context(), request); err == nil {
		t.Fatal("workflow failure not returned")
	}
	if len(executor.calls) != 1 {
		t.Fatalf("initial checks = %v", executor.calls)
	}
	workflowStore.fail.Store(false)
	result, err := service.Verify(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.Verify(t.Context(), request)
	if err != nil || !replay.Replay || !replay.ReportArtifact.Equal(result.ReportArtifact) {
		t.Fatalf("replay=%#v error=%v", replay, err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("recovery reran checks: %v", executor.calls)
	}
	conflict := request
	conflict.Requirements = append([]AcceptanceRequirement(nil), request.Requirements...)
	conflict.Requirements[0].CheckIDs = nil
	if _, err := service.Verify(t.Context(), conflict); !errors.Is(err, ErrVerificationConflict) {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestConcurrentIdenticalVerificationRunsChecksOnce(t *testing.T) {
	service, request, _, executor, _ := serviceFixture(
		t, []ExecutionMeasurement{passingMeasurement()},
	)
	const callers = 8
	results := make(chan Result, callers)
	failures := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := service.Verify(context.Background(), request)
			results <- result
			failures <- err
		}()
	}
	wait.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var artifact workflow.ArtifactRef
	for result := range results {
		if artifact.URI == "" {
			artifact = result.ReportArtifact
		}
		if !result.ReportArtifact.Equal(artifact) {
			t.Fatalf("concurrent result mismatch: %#v", result)
		}
	}
	if len(executor.calls) != 1 {
		t.Fatalf("concurrent verification ran %d checks", len(executor.calls))
	}
}

func serviceFixture(
	t *testing.T, measurements []ExecutionMeasurement,
) (*Service, Request, *fakeWorkflow, *fakeExecutor, *fakePreparer) {
	t.Helper()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	requestPB := loadImplementationRequest(t)
	requestPB.Envelope.CreatedAt = timestamppb.New(now.Add(-time.Hour))
	requestPB.Envelope.ExpiresAt = timestamppb.New(now.Add(time.Hour))
	requestPB.AvailableCheckIds = []string{"make-check-v1"}
	candidate := "cccccccccccccccccccccccccccccccccccccccc"
	report := execution.ExecutionReport{
		SchemaVersion: "1", ExecutionID: uuid.NewString(),
		ExecutedAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
		RunID:      requestPB.Envelope.RunId, TaskID: requestPB.ApprovedTaskId,
		Attempt: requestPB.Envelope.Attempt, BaseCommit: requestPB.BaseCommit,
		CandidateCommit: candidate, CandidateRef: "refs/harness/candidates/test",
	}
	reportBody, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &memoryArtifacts{values: make(map[string][]byte)}
	executionArtifact, err := artifacts.Put(t.Context(), reportBody)
	if err != nil {
		t.Fatal(err)
	}
	requirements := make([]AcceptanceRequirement, 0, len(requestPB.AcceptanceCriterionIds))
	for _, criterion := range requestPB.AcceptanceCriterionIds {
		requirements = append(requirements, AcceptanceRequirement{
			CriterionID: criterion, CheckIDs: []string{"make-check-v1"},
		})
	}
	request := Request{
		VerificationID: uuid.NewString(), VerificationTimestamp: now,
		Implementation: requestPB, ExecutionArtifact: executionArtifact,
		CandidateCommit: candidate, ExpectedTaskRevision: 4, Requirements: requirements,
	}
	workflowStore := &fakeWorkflow{}
	executor := &fakeExecutor{
		imageID:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		measurements: measurements,
	}
	preparer := &fakePreparer{root: t.TempDir()}
	service, err := NewService(Config{
		ActorID: uuid.NewString(), Catalog: DefaultCatalog(),
	}, artifacts, workflowStore, preparer, executor, NewMemoryVerificationLedger())
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	return service, request, workflowStore, executor, preparer
}

func loadImplementationRequest(t *testing.T) *reasoningv1.ImplementationRequest {
	t.Helper()
	path := filepath.Join(
		"..", "..", "..", "tests", "contracts", "v1", "implementation", "request.bin",
	)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var request reasoningv1.ImplementationRequest
	if err := proto.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	return &request
}

func passingMeasurement() ExecutionMeasurement {
	start := time.Date(2026, 7, 29, 12, 0, 1, 0, time.UTC)
	return ExecutionMeasurement{
		StartedAt: start, FinishedAt: start.Add(time.Second),
		ExitCode: 0, Output: []byte("ok\n"), WallTime: time.Second,
		UserTime: 500 * time.Millisecond, SystemTime: 100 * time.Millisecond,
		PeakRSSBytes: 1024,
	}
}

func failingMeasurement() ExecutionMeasurement {
	value := passingMeasurement()
	value.ExitCode = 2
	value.Output = []byte("failed\n")
	return value
}

func timeoutMeasurement() ExecutionMeasurement {
	value := failingMeasurement()
	value.ExitCode = -1
	value.TimedOut = true
	return value
}

func overflowMeasurement() ExecutionMeasurement {
	value := failingMeasurement()
	value.ExitCode = -1
	value.OutputTruncated = true
	return value
}
