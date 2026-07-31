package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/publication"
)

type cleanupCredential string

func (c cleanupCredential) Token(context.Context) (string, error) { return string(c), nil }

func TestCleanupResourcesReplaysAfterPartialBranchDeletion(t *testing.T) {
	root, base, candidate := cleanupGitFixture(t)
	git, err := publication.NewGitCommandPublisher(root, "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	branch := "harness/canary/run"
	if _, err := git.Publish(t.Context(), branch, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := git.DeleteBranch(t.Context(), branch, candidate); err != nil {
		t.Fatal(err)
	}
	if base == candidate {
		t.Fatal("fixture base equals candidate")
	}
	marker := "<!-- harness-publication-id:publication -->"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		state := "open"
		if request.Method == http.MethodPatch {
			state = "closed"
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"number": 7, "html_url": "https://example.invalid/pull/7", "state": state,
			"draft": true, "body": marker,
			"head": map[string]any{"ref": branch, "sha": candidate},
			"base": map[string]any{"ref": "main", "sha": base},
		})
	}))
	defer server.Close()
	github, err := publication.NewGitHubRESTClient(
		server.URL, "2022-11-28", publication.DefaultMaxBodyBytes,
		time.Second, cleanupCredential("token"),
	)
	if err != nil {
		t.Fatal(err)
	}
	branchReplay, pullReplay, err := cleanupResources(t.Context(), git, github,
		publication.PullRequestExpectation{Owner: "owner", Repo: "repo", Number: 7,
			URL: "https://example.invalid/pull/7", Marker: marker, Base: "main", Head: branch,
			BaseCommit: base, CandidateCommit: candidate})
	if err != nil || !branchReplay || pullReplay {
		t.Fatalf("partial replay branch=%v pull=%v err=%v", branchReplay, pullReplay, err)
	}
}

func TestCleanupResourcesReplaysClosedPullRequestAfterBranchConflict(t *testing.T) {
	root, base, candidate := cleanupGitFixture(t)
	git, err := publication.NewGitCommandPublisher(root, "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	branch := "harness/canary/run"
	if _, err := git.Publish(t.Context(), branch, candidate); err != nil {
		t.Fatal(err)
	}
	other := gitCommit(t, root, "other.go", "other")
	runGit(t, root, "push", "--force", "origin", other+":refs/heads/"+branch)
	marker := "<!-- harness-publication-id:publication -->"
	state := "open"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPatch {
			state = "closed"
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"number": 7, "html_url": "https://example.invalid/pull/7", "state": state,
			"draft": true, "body": marker,
			"head": map[string]any{"ref": branch, "sha": candidate},
			"base": map[string]any{"ref": "main", "sha": base},
		})
	}))
	defer server.Close()
	github, err := publication.NewGitHubRESTClient(
		server.URL, "2022-11-28", publication.DefaultMaxBodyBytes,
		time.Second, cleanupCredential("token"),
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := publication.PullRequestExpectation{Owner: "owner", Repo: "repo", Number: 7,
		URL: "https://example.invalid/pull/7", Marker: marker, Base: "main", Head: branch,
		BaseCommit: base, CandidateCommit: candidate}
	branchReplay, pullReplay, err := cleanupResources(t.Context(), git, github, expected)
	if !errors.Is(err, publication.ErrBranchConflict) || branchReplay || pullReplay || state != "closed" {
		t.Fatalf("partial cleanup branch=%v pull=%v state=%s err=%v",
			branchReplay, pullReplay, state, err)
	}
	runGit(t, root, "push", "--force", "origin", candidate+":refs/heads/"+branch)
	branchReplay, pullReplay, err = cleanupResources(t.Context(), git, github, expected)
	if err != nil || branchReplay || !pullReplay {
		t.Fatalf("cleanup replay branch=%v pull=%v err=%v", branchReplay, pullReplay, err)
	}
}

func TestCleanupResourcesDoesNotDeleteBranchWhenPullRequestCloseFails(t *testing.T) {
	root, base, candidate := cleanupGitFixture(t)
	git, err := publication.NewGitCommandPublisher(root, "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	branch := "harness/canary/run"
	if _, err := git.Publish(t.Context(), branch, candidate); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "close failed", http.StatusInternalServerError)
	}))
	defer server.Close()
	github, err := publication.NewGitHubRESTClient(
		server.URL, "2022-11-28", publication.DefaultMaxBodyBytes,
		time.Second, cleanupCredential("token"),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = cleanupResources(t.Context(), git, github, publication.PullRequestExpectation{
		Owner: "owner", Repo: "repo", Number: 7, URL: "https://example.invalid/pull/7",
		Marker: "<!-- harness-publication-id:publication -->", Base: "main", Head: branch,
		BaseCommit: base, CandidateCommit: candidate,
	})
	if err == nil {
		t.Fatal("cleanup succeeded after pull request close failure")
	}
	if head := remoteBranchHead(t, root, branch); head != candidate {
		t.Fatalf("branch changed after pull request close failure: %s", head)
	}
}

func TestWriteCleanupReportReturnsWriterError(t *testing.T) {
	errExpected := errors.New("write failed")
	err := writeCleanupReport(errorWriter{err: errExpected}, cleanupReport{Status: "cleaned"})
	if !errors.Is(err, errExpected) {
		t.Fatalf("write error = %v", err)
	}
}

type errorWriter struct{ err error }

func (writer errorWriter) Write([]byte) (int, error) { return 0, writer.err }

func cleanupGitFixture(t *testing.T) (string, string, string) {
	t.Helper()
	parent := t.TempDir()
	remote, root := filepath.Join(parent, "remote.git"), filepath.Join(parent, "source")
	runGit(t, parent, "init", "--bare", remote)
	runGit(t, parent, "init", "-b", "main", root)
	runGit(t, root, "config", "user.name", "Cleanup Test")
	runGit(t, root, "config", "user.email", "cleanup@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "add.go"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "add.go")
	runGit(t, root, "commit", "-m", "base")
	runGit(t, root, "remote", "add", "origin", remote)
	runGit(t, root, "push", "origin", "main:main")
	runGit(t, root, "fetch", "origin", "main")
	base := gitOutput(t, root, "rev-parse", "origin/main")
	if err := os.WriteFile(filepath.Join(root, "add.go"), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "commit", "-am", "candidate")
	return root, base, gitOutput(t, root, "rev-parse", "HEAD")
}

func gitCommit(t *testing.T, root, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", name)
	runGit(t, root, "commit", "-m", "other")
	return gitOutput(t, root, "rev-parse", "HEAD")
}

func remoteBranchHead(t *testing.T, root, branch string) string {
	t.Helper()
	fields := strings.Fields(gitOutput(t, root, "ls-remote", "--heads", "origin", "refs/heads/"+branch))
	if len(fields) != 2 {
		t.Fatalf("remote branch %s output = %v", branch, fields)
	}
	return fields[0]
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	if body, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, body)
	}
}

func gitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	body, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(body))
}
