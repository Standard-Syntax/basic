package verification

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestWorkspaceMaterializesExactCommitWithoutCheckoutMechanisms(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init", "-q")
	runGit(t, repository, "config", "user.name", "Test")
	runGit(t, repository, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "tracked.txt")
	runGit(t, repository, "commit", "-qm", "candidate")
	commit := runGit(t, repository, "rev-parse", "HEAD")
	marker := filepath.Join(t.TempDir(), "filter-ran")
	runGit(t, repository, "config", "filter.hostile.smudge", "touch "+marker)
	runGit(t, repository, "config", "filter.hostile.clean", "touch "+marker)
	preparer := FileWorkspacePreparer{
		RepositoryRoot: repository, WorkspaceRoot: filepath.Join(t.TempDir(), "verification"),
	}
	workspace, cleanup, err := preparer.Prepare(t.Context(), uuid.NewString(), commit)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(workspace, "tracked.txt"))
	if err != nil || string(body) != "candidate\n" {
		t.Fatalf("materialized body = %q, error = %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".git")); !os.IsNotExist(err) {
		t.Fatalf("verification workspace contains Git metadata: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("checkout filter ran: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace was not removed: %v", err)
	}
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}
