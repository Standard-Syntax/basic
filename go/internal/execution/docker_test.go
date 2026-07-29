package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerApplicatorRemovesNamedContainerOnCancellation(t *testing.T) {
	bin := t.TempDir()
	runLog := filepath.Join(bin, "run")
	removeLog := filepath.Join(bin, "remove")
	script := filepath.Join(bin, "docker")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = rm ]; then printf '%s' \"$*\" > \"$REMOVE_LOG\"; exit 0; fi\n" +
		"printf '%s' \"$*\" > \"$RUN_LOG\"\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RUN_LOG", runLog)
	t.Setenv("REMOVE_LOG", removeLog)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	worktree := t.TempDir()
	go func() {
		result <- (DockerApplicator{Image: "worker", UID: os.Getuid(), GID: os.Getgid()}).
			Apply(ctx, worktree, nil, DefaultLimits())
	}()
	waitForFile(t, runLog)
	cancel()
	err := <-result
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected cancellation error: %v", err)
	}
	runBody, readErr := os.ReadFile(runLog)
	if readErr != nil {
		t.Fatal(readErr)
	}
	runArguments := strings.Fields(string(runBody))
	name := argumentAfter(t, runArguments, "--name")
	if !containsArgument(runArguments, "-i") {
		t.Fatal("docker stdin was not attached")
	}
	removeBody, readErr := os.ReadFile(removeLog)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(removeBody) != "rm -f "+name {
		t.Fatalf("unexpected cleanup arguments: %q", removeBody)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", path)
		case <-ticker.C:
		}
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

func argumentAfter(t *testing.T, arguments []string, target string) string {
	t.Helper()
	for index, argument := range arguments {
		if argument == target && index+1 < len(arguments) {
			return arguments[index+1]
		}
	}
	t.Fatalf("argument %q not found in %v", target, arguments)
	return ""
}

func containsArgument(arguments []string, target string) bool {
	for _, argument := range arguments {
		if argument == target {
			return true
		}
	}
	return false
}
