package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
		snapshot, []string{"src"}, []string{"secret"}, ContextLimits{MaxFiles: 1, MaxBytes: 64})
	if err != nil || len(files) != 1 || files[0].Content != "package app\n" {
		t.Fatalf("context = %#v, %v", files, err)
	}
	if _, err := BuildImplementationContext(context.Background(), root, snapshot.BaseCommit,
		snapshot, []string{"src"}, nil, ContextLimits{MaxFiles: 1, MaxBytes: 2}); !errors.Is(err, ErrScopeLimit) {
		t.Fatalf("limit = %v", err)
	}
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
