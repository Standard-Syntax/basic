package execution

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Standard-Syntax/basic/go/internal/dockerengine"
)

type recordingEngine struct {
	started chan dockerengine.RunRequest
	removed chan string
}

func (*recordingEngine) ImageID(context.Context, string) (string, error) { return "", nil }
func (e *recordingEngine) Run(ctx context.Context, request dockerengine.RunRequest, _ io.Reader, _ io.Writer) error {
	e.started <- request
	<-ctx.Done()
	return ctx.Err()
}
func (e *recordingEngine) Remove(_ context.Context, name string) error { e.removed <- name; return nil }

func TestDockerApplicatorRemovesNamedContainerOnCancellation(t *testing.T) {
	engine := &recordingEngine{started: make(chan dockerengine.RunRequest, 1), removed: make(chan string, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	worktree := t.TempDir()
	go func() {
		result <- (DockerApplicator{Image: "worker", UID: os.Getuid(), GID: os.Getgid(), Engine: engine}).
			Apply(ctx, worktree, nil, DefaultLimits())
	}()
	request := <-engine.started
	cancel()
	err := <-result
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected cancellation error: %v", err)
	}
	if removed := <-engine.removed; removed != request.Name {
		t.Fatalf("removed %q; want %q", removed, request.Name)
	}
	if request.Image != "worker" || request.User == "" || len(request.Mounts) != 2 ||
		request.Mounts[0].ReadOnly || !request.Mounts[1].ReadOnly ||
		request.NanoCPUs != 1_000_000_000 || request.Memory != 512<<20 || request.Pids != 64 {
		t.Fatalf("worker request = %#v", request)
	}
}

func TestGitHelpersDoNotMixDiagnosticsIntoStdout(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "git")
	body := "#!/bin/sh\nprintf diagnostic >&2\nprintf payload\n"
	if err := os.WriteFile(script, []byte(body), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := gitOutput(t.Context(), t.TempDir(), "cat-file", "blob", "object")
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "payload" {
		t.Fatalf("git stdout was corrupted: %q", output)
	}
	output, err = gitIndexOutput(
		t.Context(), t.TempDir(), filepath.Join(t.TempDir(), "index"), nil, "hash-object",
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "payload" {
		t.Fatalf("indexed git stdout was corrupted: %q", output)
	}
}
