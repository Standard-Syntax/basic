package publication

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitPublisherPublishesExactCandidateAndReplays(t *testing.T) {
	root, remote, base, candidate := gitFixture(t)
	publisher, err := NewGitCommandPublisher(root, "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	head, err := publisher.BaseHead(t.Context())
	if err != nil || head != base {
		t.Fatalf("base head = %q, %v", head, err)
	}
	replay, err := publisher.Publish(t.Context(), "harness/run-1", candidate)
	if err != nil || replay {
		t.Fatalf("publish replay=%v err=%v", replay, err)
	}
	replay, err = publisher.Publish(t.Context(), "harness/run-1", candidate)
	if err != nil || !replay {
		t.Fatalf("retry replay=%v err=%v", replay, err)
	}
	got := gitOutput(t, remote, "rev-parse", "refs/heads/harness/run-1")
	if got != candidate {
		t.Fatalf("published head = %s", got)
	}
}

func TestGitPublisherRejectsCollisionMissingCandidateAndCancellation(t *testing.T) {
	root, _, _, candidate := gitFixture(t)
	publisher, err := NewGitCommandPublisher(root, "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(t.Context(), "harness/collision", candidate); err != nil {
		t.Fatal(err)
	}
	other := gitCommit(t, root, "other.txt", "other")
	runGit(t, root, "push", "--force", "origin", other+":refs/heads/harness/collision")
	if _, err := publisher.Publish(
		t.Context(), "harness/collision", candidate,
	); !errors.Is(err, ErrBranchConflict) {
		t.Fatalf("collision error = %v", err)
	}
	if _, err := publisher.Publish(
		t.Context(), "harness/missing", strings.Repeat("f", 40),
	); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing candidate error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := publisher.Publish(
		ctx, "harness/cancelled", other,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled publication error = %v", err)
	}
}

func TestGitPublisherDisablesHostileHooksAndPrompts(t *testing.T) {
	root, _, _, candidate := gitFixture(t)
	hooks := filepath.Join(root, ".git", "hooks")
	if err := os.WriteFile(
		filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\nexit 91\n"), 0o755,
	); err != nil {
		t.Fatal(err)
	}
	publisher, err := NewGitCommandPublisher(root, "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(t.Context(), "harness/no-hooks", candidate); err != nil {
		t.Fatalf("hostile hook was executed: %v", err)
	}
}

func TestGitPublisherDeletesOnlyExactCandidateAndReplays(t *testing.T) {
	root, remote, _, candidate := gitFixture(t)
	publisher, err := NewGitCommandPublisher(root, "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(t.Context(), "harness/canary/run", candidate); err != nil {
		t.Fatal(err)
	}
	other := gitCommit(t, root, "changed.txt", "changed")
	runGit(t, root, "push", "--force", "origin", other+":refs/heads/harness/canary/run")
	if _, err := publisher.DeleteBranch(
		t.Context(), "harness/canary/run", candidate,
	); !errors.Is(err, ErrBranchConflict) {
		t.Fatalf("changed branch deletion error = %v", err)
	}
	if head := gitOutput(t, remote, "rev-parse", "refs/heads/harness/canary/run"); head != other {
		t.Fatalf("changed branch was removed or changed: %s", head)
	}
	if _, err := publisher.DeleteBranch(t.Context(), "harness/canary/run", other); err != nil {
		t.Fatal(err)
	}
	if replay, err := publisher.DeleteBranch(
		t.Context(), "harness/canary/run", other,
	); err != nil || !replay {
		t.Fatalf("missing branch replay=%v err=%v", replay, err)
	}
}

func TestAuthenticatedGitPublisherRejectsUnsafeCredentialPaths(t *testing.T) {
	root := t.TempDir()
	unsafe := filepath.Join(root, "key with spaces")
	if err := os.WriteFile(unsafe, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAuthenticatedGitCommandPublisher(
		root, "origin", "main", unsafe,
	); !errors.Is(err, ErrCredentialPermissions) {
		t.Fatalf("unsafe credential path error = %v", err)
	}
	safe := filepath.Join(root, "deploy-key")
	if err := os.WriteFile(safe, []byte("key"), 0o640); err != nil { // skipcq: GSC-G302
		t.Fatal(err)
	}
	if _, err := NewAuthenticatedGitCommandPublisher(
		root, "origin", "main", safe,
	); !errors.Is(err, ErrCredentialPermissions) {
		t.Fatalf("broad credential mode error = %v", err)
	}
}

func gitFixture(t *testing.T) (root, remote, base, candidate string) {
	t.Helper()
	parent := t.TempDir()
	remote = filepath.Join(parent, "remote.git")
	runGit(t, parent, "init", "--bare", remote)
	root = filepath.Join(parent, "source")
	runGit(t, parent, "init", "-b", "main", root)
	runGit(t, root, "config", "user.name", "Publication Test")
	runGit(t, root, "config", "user.email", "publication@example.invalid")
	base = gitCommit(t, root, "base.txt", "base")
	runGit(t, root, "remote", "add", "origin", remote)
	runGit(t, root, "push", "origin", "main:main")
	candidate = gitCommit(t, root, "candidate.txt", "candidate")
	return root, remote, base, candidate
}

func gitCommit(t *testing.T, root, name, contents string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", name)
	runGit(t, root, "commit", "-m", name)
	return gitOutput(t, root, "rev-parse", "HEAD")
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func gitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(output))
}
