//go:build integration

package verification

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVerificationImageRunsRepositoryMakeCheckOffline(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate repository")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	candidate := integrationGit(t, repositoryRoot, "rev-parse", "HEAD")
	preparer := FileWorkspacePreparer{
		RepositoryRoot: repositoryRoot,
		WorkspaceRoot:  filepath.Join(t.TempDir(), "verification"),
	}
	workspace, cleanup, err := preparer.Prepare(t.Context(), "integration", candidate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	}()
	executor := DockerCheckExecutor{
		Image: "basic-verification-worker:integration", UID: os.Getuid(), GID: os.Getgid(),
	}
	imageID, err := executor.ImageID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := executor.Run(
		t.Context(), workspace, imageID, DefaultCatalog().definitions[0],
	)
	if err != nil {
		t.Fatal(err)
	}
	if measurement.ExitCode != 0 || measurement.TimedOut || measurement.OutputTruncated {
		t.Fatalf("make check failed: exit=%d timeout=%v overflow=%v\n%s",
			measurement.ExitCode, measurement.TimedOut,
			measurement.OutputTruncated, measurement.Output,
		)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".git")); !os.IsNotExist(err) {
		t.Fatalf("verification workspace reused Git metadata: %v", err)
	}
}

func integrationGit(t *testing.T, repository string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}
