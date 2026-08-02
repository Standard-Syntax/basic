package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/review"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

type memoryArtifacts struct {
	mu      sync.Mutex
	values  map[string][]byte
	puts    int
	failPut bool
}

func newMemoryArtifacts() *memoryArtifacts {
	return &memoryArtifacts{values: map[string][]byte{}}
}

func (s *memoryArtifacts) Put(
	_ context.Context, body []byte,
) (workflow.ArtifactRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	if s.failPut {
		s.failPut = false
		return workflow.ArtifactRef{}, errors.New("injected artifact failure")
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	s.values[digest] = append([]byte(nil), body...)
	return workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}, nil
}

func (s *memoryArtifacts) Get(
	_ context.Context, ref workflow.ArtifactRef,
) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.values[ref.Digest]
	if !ok {
		return nil, errors.New("artifact not found")
	}
	return append([]byte(nil), body...), nil
}

type workflowRecorder struct {
	mu            sync.Mutex
	command       workflow.TaskCommand
	calls         int
	seen          map[string]struct{}
	failOnce      bool
	ambiguousOnce bool
}

func (w *workflowRecorder) ExecuteTask(
	_ context.Context, command workflow.TaskCommand,
) (workflow.CommandResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failOnce {
		w.failOnce = false
		return workflow.CommandResult{}, errors.New("injected workflow failure")
	}
	if w.seen == nil {
		w.seen = make(map[string]struct{})
	}
	if _, ok := w.seen[command.Envelope().CommandID]; ok {
		return workflow.CommandResult{Replay: true}, nil
	}
	w.seen[command.Envelope().CommandID] = struct{}{}
	w.calls++
	if w.ambiguousOnce {
		w.ambiguousOnce = false
		return workflow.CommandResult{}, errors.New("ambiguous workflow completion")
	}
	w.command = command
	return workflow.CommandResult{Revision: command.Envelope().ExpectedRevision + 1}, nil
}

func approvalFixture(t *testing.T) (*Service, Request, *memoryArtifacts, *workflowRecorder) {
	t.Helper()
	store := newMemoryArtifacts()
	put := func(value string) workflow.ArtifactRef {
		ref, err := store.Put(t.Context(), []byte(value))
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	implementation := put("implementation")
	execution := put("execution")
	verification := put("verification")
	reviewRequest := put("review request")
	reviewProposal := put("review proposal")
	runID := uuid.NewString()
	taskID := uuid.NewString()
	report := review.ReviewReport{
		SchemaVersion: "1", ReviewID: uuid.NewString(),
		ReviewedAt: time.Now().UTC().Format(time.RFC3339Nano),
		RunID:      runID, TaskID: taskID, Attempt: 1,
		CandidateCommit:              "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ApprovedSpecificationDigest:  repeat('b', 64),
		ApprovedTaskDigest:           repeat('c', 64),
		ImplementationProposalDigest: implementation.Digest,
		Request:                      reviewRequest, Proposal: reviewProposal,
		Execution: execution, Verification: verification,
		Recommendation: "REVIEW_RECOMMENDATION_ADVISORY_ACCEPT", Passed: true,
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	reviewArtifact, err := store.Put(t.Context(), body)
	if err != nil {
		t.Fatal(err)
	}
	workflowPort := &workflowRecorder{}
	service, err := NewService(store, workflowPort, NewMemoryApprovalLedger())
	if err != nil {
		t.Fatal(err)
	}
	return service, Request{
		ApprovalID: uuid.NewString(), DecisionTimestamp: time.Now().UTC(),
		Principal: Principal{ID: uuid.NewString(), Roles: []Role{RoleApprover}},
		RunID:     runID, TaskID: taskID,
		CandidateCommit:             report.CandidateCommit,
		ApprovedSpecificationDigest: report.ApprovedSpecificationDigest,
		ApprovedTaskDigest:          report.ApprovedTaskDigest,
		ActualChangedPaths:          []string{"go/internal/example/service.go"},
		Implementation:              implementation, Execution: execution, Verification: verification,
		Review: reviewArtifact, ExpectedTaskRevision: 9,
	}, store, workflowPort
}

func repeat(value byte, count int) string {
	result := make([]byte, count)
	for index := range result {
		result[index] = value
	}
	return string(result)
}

func TestStandardApprovalEmitsBoundHumanCommand(t *testing.T) {
	service, request, store, workflowPort := approvalFixture(t)
	result, err := service.ApproveTask(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	command, ok := workflowPort.command.(workflow.ApproveTask)
	if !ok || command.Meta.Actor.Kind != workflow.ActorHuman ||
		!command.Approval.Equal(result.ApprovalArtifact) || result.Elevated {
		t.Fatalf("result=%#v command=%#v", result, workflowPort.command)
	}
	body, err := store.Get(t.Context(), result.ApprovalArtifact)
	if err != nil {
		t.Fatal(err)
	}
	var artifact TaskApproval
	if err := json.Unmarshal(body, &artifact); err != nil ||
		artifact.RunID != request.RunID || artifact.TaskID != request.TaskID ||
		artifact.CandidateCommit != request.CandidateCommit ||
		!artifact.Review.Equal(request.Review) ||
		artifact.Implementation.Digest != request.Implementation.Digest {
		t.Fatalf("artifact=%#v err=%v", artifact, err)
	}
}

func TestReviewReportTaskIdentityMismatchCannotTransition(t *testing.T) {
	tests := []struct {
		name string
		edit func(*review.ReviewReport)
	}{
		{
			name: "run ID",
			edit: func(report *review.ReviewReport) {
				report.RunID = "another-run"
			},
		},
		{
			name: "task ID",
			edit: func(report *review.ReviewReport) {
				report.TaskID = "TASK-999"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, request, store, workflowPort := approvalFixture(t)
			body, err := store.Get(t.Context(), request.Review)
			if err != nil {
				t.Fatal(err)
			}
			var report review.ReviewReport
			if err := json.Unmarshal(body, &report); err != nil {
				t.Fatal(err)
			}
			test.edit(&report)
			body, err = json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			request.Review, err = store.Put(t.Context(), body)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.ApproveTask(t.Context(), request); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("task identity mismatch err=%v", err)
			}
			if workflowPort.calls != 0 {
				t.Fatal("task identity mismatch reached workflow")
			}
		})
	}
}

func TestElevatedApprovalRequiresElevatedRole(t *testing.T) {
	service, request, _, workflowPort := approvalFixture(t)
	request.ActualChangedPaths = []string{"go.mod"}
	if _, err := service.ApproveTask(t.Context(), request); !errors.Is(err, ErrElevatedRole) {
		t.Fatalf("elevated approval err=%v", err)
	}
	if workflowPort.calls != 0 {
		t.Fatal("unauthorized elevated approval reached workflow")
	}
	request.Principal.Roles = []Role{RoleElevatedApprover}
	result, err := service.ApproveTask(t.Context(), request)
	if err != nil || !result.Elevated || workflowPort.calls != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestEitherApproverRoleMayRequireRework(t *testing.T) {
	service, request, _, workflowPort := approvalFixture(t)
	request.ExclusiveResourceLabels = []string{"database-schema"}
	result, err := service.RequireTaskRework(t.Context(), request, "needs a safe migration")
	if err != nil {
		t.Fatal(err)
	}
	command, ok := workflowPort.command.(workflow.RequireTaskRework)
	if !ok || command.Meta.Actor.Kind != workflow.ActorHuman || !result.Elevated {
		t.Fatalf("result=%#v command=%#v", result, workflowPort.command)
	}
}

func TestReviewerIdentityCannotBecomeHumanAuthority(t *testing.T) {
	service, request, _, workflowPort := approvalFixture(t)
	request.Principal = Principal{ID: uuid.NewString()}
	if _, err := service.ApproveTask(t.Context(), request); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("reviewer authority err=%v", err)
	}
	if workflowPort.calls != 0 {
		t.Fatal("reviewer identity emitted a human workflow command")
	}
}

func TestElevatedRiskClassificationIsDeterministic(t *testing.T) {
	reasons := ClassifyElevatedRisk(
		[]string{
			"go.mod", "proto/harness/v1/api.proto", ".github/workflows/check.yml",
			"db/migrations/0013_change.sql", "Dockerfile.worker", "go/ordinary.go",
		},
		[]string{"public-api-contract", "ordinary-resource"},
	)
	want := []string{
		"path:.github/workflows/check.yml", "path:Dockerfile.worker",
		"path:db/migrations/0013_change.sql", "path:go.mod",
		"path:proto/harness/v1/api.proto", "resource:public-api-contract",
	}
	if len(reasons) != len(want) {
		t.Fatalf("risk reasons=%v", reasons)
	}
	for index := range want {
		if reasons[index] != want[index] {
			t.Fatalf("risk reasons=%v", reasons)
		}
	}
}

func TestConcurrentExactApprovalIsOneLogicalDecision(t *testing.T) {
	service, request, _, workflowPort := approvalFixture(t)
	const callers = 12
	results := make(chan Result, callers)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			result, err := service.ApproveTask(context.Background(), request)
			results <- result
			errs <- err
		}()
	}
	var artifact workflow.ArtifactRef
	for range callers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		result := <-results
		if artifact.URI == "" {
			artifact = result.ApprovalArtifact
		}
		if !artifact.Equal(result.ApprovalArtifact) {
			t.Fatalf("concurrent artifacts differ: %v %v", artifact, result.ApprovalArtifact)
		}
	}
	if workflowPort.calls != 1 {
		t.Fatalf("logical workflow decisions=%d", workflowPort.calls)
	}
}

func TestConflictingDecisionReuseFailsClosed(t *testing.T) {
	service, request, _, workflowPort := approvalFixture(t)
	if _, err := service.ApproveTask(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RequireTaskRework(
		t.Context(), request, "different decision",
	); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("conflicting decision err=%v", err)
	}
	if workflowPort.calls != 1 {
		t.Fatalf("conflict emitted workflow command: %d", workflowPort.calls)
	}
}

func TestDecisionReadyRecoveryRetriesOnlyWorkflow(t *testing.T) {
	service, request, store, workflowPort := approvalFixture(t)
	workflowPort.failOnce = true
	if _, err := service.ApproveTask(t.Context(), request); err == nil {
		t.Fatal("injected workflow failure was hidden")
	}
	puts := store.puts
	result, err := service.ApproveTask(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if store.puts != puts || result.ApprovalArtifact.URI == "" || workflowPort.calls != 1 {
		t.Fatalf("recovery repeated pre-transition work: puts=%d/%d calls=%d", store.puts, puts, workflowPort.calls)
	}
}

func TestArtifactFailureRollsBackWithoutWorkflow(t *testing.T) {
	service, request, store, workflowPort := approvalFixture(t)
	store.failPut = true
	if _, err := service.ApproveTask(t.Context(), request); err == nil {
		t.Fatal("artifact failure was hidden")
	}
	if workflowPort.calls != 0 {
		t.Fatal("artifact failure reached workflow")
	}
	if _, err := service.ApproveTask(t.Context(), request); err != nil {
		t.Fatalf("reservation did not roll back: %v", err)
	}
}

func TestAmbiguousWorkflowCompletionRecoversByIdempotentReplay(t *testing.T) {
	service, request, store, workflowPort := approvalFixture(t)
	workflowPort.ambiguousOnce = true
	if _, err := service.ApproveTask(t.Context(), request); err == nil {
		t.Fatal("ambiguous completion was hidden")
	}
	puts := store.puts
	result, err := service.ApproveTask(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Replay || store.puts != puts || workflowPort.calls != 1 {
		t.Fatalf("recovery result=%#v puts=%d/%d calls=%d", result, store.puts, puts, workflowPort.calls)
	}
}

func TestPhase5Through8HumanApprovalBoundary(t *testing.T) {
	service, request, _, workflowPort := approvalFixture(t)
	result, err := service.ApproveTask(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	command := workflowPort.command.(workflow.ApproveTask)
	now := request.DecisionTimestamp.Add(-time.Minute)
	task := workflow.Task{
		ID: request.TaskID, RunID: request.RunID,
		State: workflow.TaskStateAwaitingApproval, Revision: request.ExpectedTaskRevision,
		MaxAttempts: 1, CurrentAttempt: 1,
		Proposal: &request.Implementation, Execution: &request.Execution,
		Verification: &request.Verification, Review: &request.Review,
		CandidateCommit: request.CandidateCommit, CreatedAt: now, UpdatedAt: now,
	}
	accepted, events, err := task.Apply(command)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.State != workflow.TaskStateAccepted || accepted.Approval == nil ||
		!accepted.Approval.Equal(result.ApprovalArtifact) ||
		len(events) != 1 || events[0].Type != "TASK_ACCEPTED" {
		t.Fatalf("accepted=%#v events=%#v", accepted, events)
	}
}

func TestCandidateOrReviewDigestMismatchCannotTransition(t *testing.T) {
	service, request, _, workflowPort := approvalFixture(t)
	request.CandidateCommit = repeat('9', 40)
	if _, err := service.ApproveTask(t.Context(), request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("candidate mismatch err=%v", err)
	}
	request.CandidateCommit = repeat('a', 40)
	request.Review.Digest = repeat('8', 64)
	request.Review.URI = "artifact://sha256/" + request.Review.Digest
	if _, err := service.ApproveTask(t.Context(), request); err == nil {
		t.Fatal("altered review digest was accepted")
	}
	if workflowPort.calls != 0 {
		t.Fatal("mismatched evidence reached workflow")
	}
}
