//go:build integration

package execution

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

func TestDockerWorkerProducesCandidateWithoutHooksFiltersOrNetwork(t *testing.T) {
	if _, err := os.Stat("/var/run/docker.sock"); err != nil {
		t.Skip("Docker socket is unavailable")
	}
	request, proposal, proposalBody := executionFixture(t)
	repository, worktrees, _ := fixtureRepository(t, request)
	marker := filepath.Join(filepath.Dir(repository), "hook-or-filter-ran")
	attributes := filepath.Join(repository, ".gitattributes")
	if err := os.WriteFile(
		attributes, []byte("go/internal/reasoning/existing.go filter=evil\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", ".gitattributes")
	runGit(t, repository, "commit", "-qm", "add hostile attributes")
	hook := filepath.Join(repository, ".git", "hooks", "post-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "config", "filter.evil.smudge", "touch "+marker)
	runGit(t, repository, "config", "filter.evil.clean", "touch "+marker)
	request.BaseCommit = runGit(t, repository, "rev-parse", "HEAD")
	digest := sha256Hex(proposalBody)
	artifact := workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}
	now := request.GetEnvelope().GetCreatedAt().AsTime().Add(time.Minute)
	service, err := NewService(Config{
		RepositoryRoot: repository, WorktreeRoot: worktrees,
		WorkerImage: "basic-execution-worker:integration",
		UID:         os.Getuid(), GID: os.Getgid(), Limits: DefaultLimits(),
		ActorID: uuid.NewString(), AuthorName: "Harness Execution",
		AuthorEmail: "execution@harness.invalid",
	}, memoryArtifacts{digest: proposalBody}, DockerApplicator{
		Image: "basic-execution-worker:integration", UID: os.Getuid(), GID: os.Getgid(),
	}, &fakeWorkflow{}, NewMemoryExecutionLedger())
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	result, err := service.Execute(t.Context(), Request{
		ExecutionID: uuid.NewString(), ExecutionTimestamp: now,
		Implementation: request, Proposal: proposal, ProposalArtifact: artifact,
		Lease: workflow.LeaseRef{
			ID: uuid.NewString(), OwnerID: uuid.NewString(),
			ExpiresAt: now.Add(time.Hour), FencingToken: request.GetEnvelope().GetAttempt(),
		},
		ExpectedTaskRevision: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.CandidateCommit == "" || len(result.ActualDiff) != 3 {
		t.Fatalf("unexpected Docker result: %#v", result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository hook or filter executed: %v", err)
	}
	sourceBody, err := os.ReadFile(
		filepath.Join(repository, "go", "internal", "reasoning", "existing.go"),
	)
	if err != nil || string(sourceBody) != request.GetRepositoryContext()[0].GetContent() {
		t.Fatalf("source checkout changed: %q %v", sourceBody, err)
	}
}
