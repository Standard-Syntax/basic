// Command verification-worker runs a kernel-resolved check in the isolated image.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, request.Argv[0], request.Argv[1:]...)
	command.Dir = "/workspace"
	command.Env = []string{
		"HOME=/tmp/home", "TMPDIR=/tmp", "GOCACHE=/tmp/go-build",
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
