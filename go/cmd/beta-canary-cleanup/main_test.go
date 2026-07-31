package main

import (
	"context"
	"encoding/json"
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
	root, candidate := cleanupGitFixture(t)
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
	base := gitOutput(t, root, "rev-parse", "main")
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

func cleanupGitFixture(t *testing.T) (string, string) {
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
	if err := os.WriteFile(filepath.Join(root, "add.go"), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "commit", "-am", "candidate")
	return root, gitOutput(t, root, "rev-parse", "HEAD")
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
