package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

type memoryArtifacts map[string][]byte

func (m memoryArtifacts) Get(_ context.Context, reference workflow.ArtifactRef) ([]byte, error) {
	value, ok := m[reference.Digest]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), value...), nil
}

func (m memoryArtifacts) Put(_ context.Context, body []byte) (workflow.ArtifactRef, error) {
	digest := sha256Hex(body)
	m[digest] = append([]byte(nil), body...)
	return workflow.ArtifactRef{
		URI: "artifact://sha256/" + digest, Digest: digest,
	}, nil
}

type fakeWorkflow struct {
	commands []workflow.TaskCommand
	failLast error
	replay   bool
}

func (f *fakeWorkflow) ExecuteTask(
	_ context.Context, command workflow.TaskCommand,
) (workflow.CommandResult, error) {
	f.commands = append(f.commands, command)
	if _, final := command.(workflow.RecordTaskExecution); final && f.failLast != nil {
		return workflow.CommandResult{}, f.failLast
	}
	return workflow.CommandResult{
		Revision: command.Envelope().ExpectedRevision + 1, Replay: f.replay,
	}, nil
}

type localApplicator struct {
	calls atomic.Int32
}

func (a *localApplicator) Apply(
	_ context.Context, worktree string, changes []contracts.FileChange, _ Limits,
) error {
	a.calls.Add(1)
	for _, change := range changes {
		target := filepath.Join(worktree, filepath.FromSlash(change.Path))
		switch change.Operation {
		case contracts.FileDelete:
			if err := os.Remove(target); err != nil {
				return err
			}
		case contracts.FileCreate, contracts.FileUpdate:
			mode := os.FileMode(0o644)
			if info, err := os.Stat(target); err == nil {
				mode = info.Mode().Perm()
			}
			if err := os.WriteFile(target, []byte(*change.ReplacementContent), mode); err != nil {
				return err
			}
		}
	}
	return nil
}

func executionFixture(
	t *testing.T,
) (*reasoningv1.ImplementationRequest, *reasoningv1.ImplementationProposal, []byte) {
	t.Helper()
	root := filepath.Join("..", "..", "..", "tests", "contracts", "v1", "implementation")
	requestBody, err := os.ReadFile(filepath.Join(root, "request.bin"))
	if err != nil {
		t.Fatal(err)
	}
	proposalBody, err := os.ReadFile(filepath.Join(root, "proposal.bin"))
	if err != nil {
		t.Fatal(err)
	}
	var request reasoningv1.ImplementationRequest
	var proposal reasoningv1.ImplementationProposal
	if err := proto.Unmarshal(requestBody, &request); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(proposalBody, &proposal); err != nil {
		t.Fatal(err)
	}
	proposal.Changes[2].ExpectedOriginalSha256 = sha256Hex([]byte("package reasoning\n"))
	proposalBody, err = proto.MarshalOptions{Deterministic: true}.Marshal(&proposal)
	if err != nil {
		t.Fatal(err)
	}
	return &request, &proposal, proposalBody
}

func fixtureRepository(
	t *testing.T, request *reasoningv1.ImplementationRequest,
) (repository, worktrees, commit string) {
	t.Helper()
	root := t.TempDir()
	repository, worktrees = filepath.Join(root, "repository"), filepath.Join(root, "worktrees")
	if err := os.Mkdir(repository, 0o750); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init", "-q")
	runGit(t, repository, "config", "user.name", "Fixture")
	runGit(t, repository, "config", "user.email", "fixture@example.invalid")
	for _, file := range request.GetRepositoryContext() {
		target := filepath.Join(repository, filepath.FromSlash(file.GetPath()))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(file.GetContent()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	obsolete := filepath.Join(repository, "go", "internal", "reasoning", "obsolete.go")
	if err := os.MkdirAll(filepath.Dir(obsolete), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obsolete, []byte("package reasoning\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", ".")
	runGit(t, repository, "commit", "-qm", "fixture")
	commit = runGit(t, repository, "rev-parse", "HEAD")
	return repository, worktrees, commit
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return string(bytesTrimSpace(output))
}

func bytesTrimSpace(value []byte) []byte {
	for len(value) > 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r') {
		value = value[:len(value)-1]
	}
	return value
}

func executeConcurrently(t *testing.T, service *Service, request Request) []Result {
	t.Helper()
	var wait sync.WaitGroup
	results := make([]Result, 2)
	executionErrors := make([]error, 2)
	for index := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[index], executionErrors[index] = service.Execute(t.Context(), request)
		}()
	}
	wait.Wait()
	for _, err := range executionErrors {
		if err != nil {
			t.Fatal(err)
		}
	}
	return results
}

func requirePrimaryResult(t *testing.T, results []Result) Result {
	t.Helper()
	if results[0].Replay == results[1].Replay {
		t.Fatalf("concurrent results did not converge to one replay: %#v", results)
	}
	if results[0].Replay {
		return results[1]
	}
	return results[0]
}

func requireExecutionResult(
	t *testing.T,
	result Result,
	repository, commit string,
	proposal *reasoningv1.ImplementationProposal,
	applicator *localApplicator,
	workflowStore *fakeWorkflow,
) {
	t.Helper()
	if applicator.calls.Load() != 1 || result.BaseCommit != commit ||
		len(workflowStore.commands) != 2 || result.CandidateCommit == "" {
		t.Fatalf("unexpected result: %#v calls=%d", result, applicator.calls.Load())
	}
	if len(result.ActualDiff) != len(proposal.GetChanges()) {
		t.Fatalf("actual diff = %#v", result.ActualDiff)
	}
	if resolved := runGit(t, repository, "rev-parse", result.CandidateRef); resolved != result.CandidateCommit {
		t.Fatalf("candidate ref = %s, want %s", resolved, result.CandidateCommit)
	}
}

func requireReplayAndConflict(
	t *testing.T,
	service *Service,
	request Request,
	result Result,
	applicator *localApplicator,
) {
	t.Helper()
	repeated, err := service.Execute(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.CandidateCommit != result.CandidateCommit ||
		!repeated.ReportArtifact.Equal(result.ReportArtifact) || !repeated.Replay {
		t.Fatalf("execution was not deterministic: %#v %#v", result, repeated)
	}
	conflict := request
	conflict.ExpectedTaskRevision++
	if _, err := service.Execute(t.Context(), conflict); !errors.Is(err, ErrExecutionConflict) {
		t.Fatalf("conflicting execution error = %v", err)
	}
	if applicator.calls.Load() != 1 {
		t.Fatalf("conflicting execution reached applicator %d times", applicator.calls.Load())
	}
}

func TestServiceRevalidatesMaterializesAndAppliesProposal(t *testing.T) {
	request, proposal, proposalBody := executionFixture(t)
	repository, worktrees, commit := fixtureRepository(t, request)
	request.BaseCommit = commit
	sum := sha256.Sum256(proposalBody)
	digest := hex.EncodeToString(sum[:])
	artifact := workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}
	applicator := &localApplicator{}
	workflowStore := &fakeWorkflow{}
	service, err := NewService(Config{
		RepositoryRoot: repository, WorktreeRoot: worktrees, WorkerImage: "test",
		UID: os.Getuid(), GID: os.Getgid(), Limits: DefaultLimits(),
		ActorID: uuid.NewString(), AuthorName: "Harness Execution",
		AuthorEmail: "execution@harness.invalid",
	}, memoryArtifacts{digest: proposalBody}, applicator, workflowStore, NewMemoryExecutionLedger())
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return request.GetEnvelope().GetCreatedAt().AsTime().Add(time.Minute) }
	lease := workflow.LeaseRef{
		ID: uuid.NewString(), OwnerID: uuid.NewString(),
		ExpiresAt:    service.now().Add(time.Hour),
		FencingToken: request.GetEnvelope().GetAttempt(),
	}
	executionRequest := Request{
		ExecutionID: uuid.NewString(), ExecutionTimestamp: service.now(),
		Implementation: request, Proposal: proposal,
		ProposalArtifact: artifact, Lease: lease, ExpectedTaskRevision: 3,
	}
	result := requirePrimaryResult(t, executeConcurrently(t, service, executionRequest))
	requireExecutionResult(
		t, result, repository, commit, proposal, applicator, workflowStore,
	)
	workflowStore.replay = true
	requireReplayAndConflict(t, service, executionRequest, result, applicator)
	if status := runGit(t, repository, "status", "--short"); status != "" {
		t.Fatalf("source checkout changed: %s", status)
	}
}

func TestServiceRejectsArtifactAndPathBeforeApplicator(t *testing.T) {
	request, proposal, proposalBody := executionFixture(t)
	repository, worktrees, commit := fixtureRepository(t, request)
	request.BaseCommit = commit
	sum := sha256.Sum256(proposalBody)
	digest := hex.EncodeToString(sum[:])
	artifact := workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}
	applicator := &localApplicator{}
	workflowStore := &fakeWorkflow{}
	service, err := NewService(Config{
		RepositoryRoot: repository, WorktreeRoot: worktrees, WorkerImage: "test",
		Limits: DefaultLimits(), ActorID: uuid.NewString(),
		AuthorName: "Harness Execution", AuthorEmail: "execution@harness.invalid",
	}, memoryArtifacts{digest: []byte("corrupt")}, applicator, workflowStore, NewMemoryExecutionLedger())
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return request.GetEnvelope().GetCreatedAt().AsTime().Add(time.Minute) }
	lease := workflow.LeaseRef{
		ID: uuid.NewString(), OwnerID: uuid.NewString(),
		ExpiresAt:    service.now().Add(time.Hour),
		FencingToken: request.GetEnvelope().GetAttempt(),
	}
	_, err = service.Execute(t.Context(), Request{
		ExecutionID: uuid.NewString(), ExecutionTimestamp: service.now(),
		Implementation: request, Proposal: proposal,
		ProposalArtifact: artifact, Lease: lease, ExpectedTaskRevision: 3,
	})
	if !errors.Is(err, ErrArtifactIntegrity) || applicator.calls.Load() != 0 {
		t.Fatalf("corrupt proposal reached applicator: calls=%d err=%v", applicator.calls.Load(), err)
	}

	replacement := "content"
	unsafe := []contracts.FileChange{
		{Path: ".git/config", Operation: contracts.FileCreate, ReplacementContent: &replacement},
		{Path: "a/../b", Operation: contracts.FileCreate, ReplacementContent: &replacement},
		{Path: "dup", Operation: contracts.FileCreate, ReplacementContent: &replacement},
		{Path: "dup", Operation: contracts.FileCreate, ReplacementContent: &replacement},
	}
	if err := preflightChanges(unsafe, DefaultLimits()); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("unsafe changes error = %v", err)
	}
}

func TestFinalWorkflowFailureRemovesCandidateRef(t *testing.T) {
	request, proposal, proposalBody := executionFixture(t)
	repository, worktrees, commit := fixtureRepository(t, request)
	request.BaseCommit = commit
	digest := sha256Hex(proposalBody)
	artifact := workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}
	now := request.GetEnvelope().GetCreatedAt().AsTime().Add(time.Minute)
	lease := workflow.LeaseRef{
		ID: uuid.NewString(), OwnerID: uuid.NewString(),
		ExpiresAt: now.Add(time.Hour), FencingToken: request.GetEnvelope().GetAttempt(),
	}
	workflowStore := &fakeWorkflow{failLast: workflow.ErrRevisionConflict}
	service, err := NewService(Config{
		RepositoryRoot: repository, WorktreeRoot: worktrees, WorkerImage: "test",
		Limits: DefaultLimits(), ActorID: uuid.NewString(),
		AuthorName: "Harness Execution", AuthorEmail: "execution@harness.invalid",
	}, memoryArtifacts{digest: proposalBody}, &localApplicator{}, workflowStore, NewMemoryExecutionLedger())
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	executionRequest := Request{
		ExecutionID: uuid.NewString(), ExecutionTimestamp: now,
		Implementation: request, Proposal: proposal, ProposalArtifact: artifact,
		Lease: lease, ExpectedTaskRevision: 3,
	}
	_, err = service.Execute(t.Context(), executionRequest)
	if !errors.Is(err, workflow.ErrRevisionConflict) {
		t.Fatalf("final workflow error = %v", err)
	}
	ref := "refs/harness/candidates/" + request.GetEnvelope().GetRunId() + "/" +
		request.GetApprovedTaskId() + "/1-1"
	command := exec.Command("git", "-C", repository, "show-ref", "--verify", "--quiet", ref)
	if command.Run() == nil {
		t.Fatalf("stale candidate ref %q remains", ref)
	}
	workflowStore.failLast = nil
	executionRequest.ExecutionID = uuid.NewString()
	result, err := service.Execute(t.Context(), executionRequest)
	if err != nil {
		t.Fatal(err)
	}
	workflowStore.failLast = workflow.ErrRevisionConflict
	executionRequest.ExecutionID = uuid.NewString()
	if _, err := service.Execute(t.Context(), executionRequest); !errors.Is(
		err, workflow.ErrRevisionConflict,
	) {
		t.Fatalf("replayed final workflow error = %v", err)
	}
	if resolved := runGit(t, repository, "rev-parse", ref); resolved != result.CandidateCommit {
		t.Fatalf("pre-existing candidate ref was removed: got %q want %q", resolved, result.CandidateCommit)
	}
}
