package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

type localApplicator struct {
	calls int
}

func (a *localApplicator) Apply(
	_ context.Context, worktree string, changes []contracts.FileChange, _ Limits,
) error {
	a.calls++
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

func TestServiceRevalidatesMaterializesAndAppliesProposal(t *testing.T) {
	request, proposal, proposalBody := executionFixture(t)
	repository, worktrees, commit := fixtureRepository(t, request)
	request.BaseCommit = commit
	sum := sha256.Sum256(proposalBody)
	digest := hex.EncodeToString(sum[:])
	artifact := workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}
	applicator := &localApplicator{}
	service, err := NewService(Config{
		RepositoryRoot: repository, WorktreeRoot: worktrees, WorkerImage: "test",
		UID: os.Getuid(), GID: os.Getgid(), Limits: DefaultLimits(),
	}, memoryArtifacts{digest: proposalBody}, applicator)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return request.GetEnvelope().GetCreatedAt().AsTime().Add(time.Minute) }
	lease := workflow.LeaseRef{
		ID: uuid.NewString(), OwnerID: uuid.NewString(),
		ExpiresAt:    service.now().Add(time.Hour),
		FencingToken: request.GetEnvelope().GetAttempt(),
	}
	result, err := service.Execute(t.Context(), Request{
		ExecutionID: uuid.NewString(), Implementation: request, Proposal: proposal,
		ProposalArtifact: artifact, Lease: lease, ExpectedTaskRevision: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = removeWorktree(context.Background(), repository, result.Worktree)
	})
	if applicator.calls != 1 || result.BaseCommit != commit {
		t.Fatalf("unexpected result: %#v calls=%d", result, applicator.calls)
	}
	for _, change := range result.Proposal.Changes {
		target := filepath.Join(result.Worktree, filepath.FromSlash(change.Path))
		body, readErr := os.ReadFile(target)
		switch change.Operation {
		case contracts.FileDelete:
			if !errors.Is(readErr, os.ErrNotExist) {
				t.Fatalf("deleted path %q remains: %v", change.Path, readErr)
			}
		default:
			if readErr != nil || string(body) != *change.ReplacementContent {
				t.Fatalf("replacement %q mismatch: %q %v", change.Path, body, readErr)
			}
		}
	}
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
	service, err := NewService(Config{
		RepositoryRoot: repository, WorktreeRoot: worktrees, WorkerImage: "test",
		Limits: DefaultLimits(),
	}, memoryArtifacts{digest: []byte("corrupt")}, applicator)
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
		ExecutionID: uuid.NewString(), Implementation: request, Proposal: proposal,
		ProposalArtifact: artifact, Lease: lease, ExpectedTaskRevision: 3,
	})
	if !errors.Is(err, ErrArtifactIntegrity) || applicator.calls != 0 {
		t.Fatalf("corrupt proposal reached applicator: calls=%d err=%v", applicator.calls, err)
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
