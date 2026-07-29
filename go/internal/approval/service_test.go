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
	mu     sync.Mutex
	values map[string][]byte
}

func newMemoryArtifacts() *memoryArtifacts {
	return &memoryArtifacts{values: map[string][]byte{}}
}

func (s *memoryArtifacts) Put(
	_ context.Context, body []byte,
) (workflow.ArtifactRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	command workflow.TaskCommand
	calls   int
}

func (w *workflowRecorder) ExecuteTask(
	_ context.Context, command workflow.TaskCommand,
) (workflow.CommandResult, error) {
	w.calls++
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
	report := review.ReviewReport{
		SchemaVersion: "1", ReviewID: uuid.NewString(),
		ReviewedAt: time.Now().UTC().Format(time.RFC3339Nano),
		RunID:      "run-1", TaskID: "TASK-001", Attempt: 1,
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
	service, err := NewService(store, workflowPort)
	if err != nil {
		t.Fatal(err)
	}
	return service, Request{
		ApprovalID: uuid.NewString(), DecisionTimestamp: time.Now().UTC(),
		Principal: Principal{ID: uuid.NewString(), Roles: []Role{RoleApprover}},
		RunID:     "run-1", TaskID: "TASK-001",
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
		artifact.CandidateCommit != request.CandidateCommit ||
		!artifact.Review.Equal(request.Review) ||
		artifact.Implementation.Digest != request.Implementation.Digest {
		t.Fatalf("artifact=%#v err=%v", artifact, err)
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
