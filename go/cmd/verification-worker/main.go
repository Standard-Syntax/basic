// Command verification-worker runs a kernel-resolved check in the isolated image.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/verification"
)

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(input io.Reader, output io.Writer) error {
	request, err := decode(input)
	if err != nil {
		return err
	}
	timeout := time.Duration(request.TimeoutNanos)
	if request.CommandReference != "make-check-v1" ||
		!reflect.DeepEqual(request.Argv, []string{"make", "check"}) ||
		timeout <= 0 || timeout > verification.DefaultCheckTimeout ||
		request.OutputBytes <= 0 || request.OutputBytes > verification.DefaultMaxOutputBytes {
		return errors.New("unapproved verification command")
	}
	if err := prepareRuntimeCaches(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, request.Argv[0], request.Argv[1:]...)
	command.Dir = "/workspace"
	command.Env = []string{
		"HOME=/tmp/home", "TMPDIR=/tmp", "GOCACHE=/tmp/go-build",
		"GOMODCACHE=/go/pkg/mod", "GOPROXY=off", "GOSUMDB=off",
		"UV_CACHE_DIR=/tmp/uv-cache", "UV_OFFLINE=1", "UV_NO_SYNC=1",
		"UV_PROJECT_ENVIRONMENT=/opt/venv", "PYTHONPATH=/workspace/python/src",
		"PATH=/opt/bin:/opt/venv/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin",
		"LANG=C.UTF-8", "LC_ALL=C.UTF-8",
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 5 * time.Second
	var combined boundedBuffer
	combined.limit = request.OutputBytes
	command.Stdout, command.Stderr = &combined, &combined
	started := time.Now().UTC()
	runErr := command.Run()
	finished := time.Now().UTC()
	response := verification.WorkerResponse{
		StartedAt: started, FinishedAt: finished, ExitCode: exitCode(runErr),
		TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
		Output:   combined.buffer.Bytes(), OutputTruncated: combined.overflow,
		WallTimeNanos: finished.Sub(started).Nanoseconds(),
	}
	if response.TimedOut || combined.overflow {
		response.ExitCode = -1
	}
	if state := command.ProcessState; state != nil {
		response.UserTimeNanos = state.UserTime().Nanoseconds()
		response.SystemTimeNanos = state.SystemTime().Nanoseconds()
		if usage, ok := state.SysUsage().(*syscall.Rusage); ok {
			response.PeakRSSBytes = usage.Maxrss * 1024
		}
	}
	return json.NewEncoder(output).Encode(response)
}

func prepareRuntimeCaches() error {
	if err := os.MkdirAll("/tmp/uv-cache", 0o700); err != nil {
		return fmt.Errorf("create writable uv cache: %w", err)
	}
	uvCopy := exec.Command("/bin/cp", "-a", "/opt/uv-cache/.", "/tmp/uv-cache/")
	uvCopy.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	if output, err := uvCopy.CombinedOutput(); err != nil {
		return fmt.Errorf("seed writable uv cache: %w: %s", err, bytes.TrimSpace(output))
	}
	if err := secureRuntimeCache("/tmp/uv-cache"); err != nil {
		return fmt.Errorf("secure writable uv cache: %w", err)
	}
	if err := os.MkdirAll("/tmp/go-build", 0o700); err != nil {
		return fmt.Errorf("create writable Go cache: %w", err)
	}
	command := exec.Command("/bin/cp", "-a", "/opt/go-build-cache/.", "/tmp/go-build/")
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("seed writable Go cache: %w: %s", err, bytes.TrimSpace(output))
	}
	if err := secureRuntimeCache("/tmp/go-build"); err != nil {
		return fmt.Errorf("secure writable Go cache: %w", err)
	}
	return nil
}

func secureRuntimeCache(root string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve cache root: %w", err)
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, resolveErr := filepath.EvalSymlinks(path)
			if resolveErr != nil {
				return fmt.Errorf("resolve cache symbolic link %q: %w", path, resolveErr)
			}
			relative, relativeErr := filepath.Rel(resolvedRoot, target)
			if relativeErr != nil || !filepath.IsLocal(relative) {
				return fmt.Errorf("cache symbolic link escapes root %q", path)
			}
			return nil
		}
		mode := os.FileMode(0o600)
		if entry.IsDir() {
			mode |= 0o100
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("set cache path permissions: %w", err)
		}
		return nil
	})
}

func decode(input io.Reader) (verification.WorkerRequest, error) {
	decoder := json.NewDecoder(io.LimitReader(input, 64*1024))
	decoder.DisallowUnknownFields()
	var value verification.WorkerRequest
	if err := decoder.Decode(&value); err != nil {
		return verification.WorkerRequest{}, fmt.Errorf("decode request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return verification.WorkerRequest{}, errors.New("invalid request trailer")
	}
	return value, nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	overflow bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	remaining := b.limit - int64(b.buffer.Len())
	if remaining > 0 {
		count := int64(len(value))
		if count > remaining {
			count = remaining
		}
		_, _ = b.buffer.Write(value[:count])
	}
	if int64(len(value)) > remaining {
		b.overflow = true
	}
	return len(value), nil
}
