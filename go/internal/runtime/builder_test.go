package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
)

func TestRepositorySnapshotAndBoundedContextUseCommittedObjects(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.name", "Test")
	runGit(t, root, "config", "user.email", "test@example.invalid")
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "app.go"), []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret"), []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "fixture")
	snapshot, err := SnapshotRepository(context.Background(), root, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "app.go"), []byte("untracked mutation"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := BuildImplementationContext(context.Background(), root, snapshot.BaseCommit,
		snapshot, []string{"src"}, []string{"src/app.go"}, ContextLimits{MaxFiles: 1, MaxBytes: 64})
	if err != nil || len(files) != 1 || files[0].Content != "package app\n" {
		t.Fatalf("context = %#v, %v", files, err)
	}
	if _, err := BuildImplementationContext(context.Background(), root, snapshot.BaseCommit,
		snapshot, []string{"src"}, nil, ContextLimits{MaxFiles: 1, MaxBytes: 2}); !errors.Is(err, ErrScopeLimit) {
		t.Fatalf("limit = %v", err)
	}
}

func TestReadContextBatchConsumesOutputWhileWritingRequests(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.name", "Test")
	runGit(t, root, "config", "user.email", "test@example.invalid")
	body := bytes.Repeat([]byte("x"), 8<<10)
	if err := os.WriteFile(filepath.Join(root, "blob"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "blob")
	runGit(t, root, "commit", "-qm", "pipe fixture")
	snapshot, err := SnapshotRepository(t.Context(), root, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]*reasoningv1.RepositoryEntry, 2048)
	for index := range entries {
		entries[index] = snapshot.Entries[0]
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	files, err := readContextBatch(ctx, root, snapshot.BaseCommit, entries, 20<<20)
	if err != nil || len(files) != len(entries) {
		t.Fatalf("batch files=%d err=%v", len(files), err)
	}
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
