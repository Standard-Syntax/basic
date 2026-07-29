package verification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var imageIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type DockerCheckExecutor struct {
	Image string
	UID   int
	GID   int
}

func (d DockerCheckExecutor) ImageID(ctx context.Context) (string, error) {
	if d.Image == "" || d.UID <= 0 || d.GID <= 0 {
		return "", errors.New("verification image and non-root UID/GID are required")
	}
	output, err := exec.CommandContext(
		ctx, "docker", "image", "inspect", "--format", "{{.Id}}", d.Image,
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("inspect verification image: %w: %s", err, strings.TrimSpace(string(output)))
	}
	imageID := strings.TrimSpace(string(output))
	if !imageIDPattern.MatchString(imageID) {
		return "", fmt.Errorf("%w: image identity", ErrWorkerResponse)
	}
	return imageID, nil
}

func (d DockerCheckExecutor) Run(
	ctx context.Context, workspace string, definition CheckDefinition,
) (ExecutionMeasurement, error) {
	imageID, err := d.ImageID(ctx)
	if err != nil {
		return ExecutionMeasurement{}, err
	}
	payload, err := json.Marshal(WorkerRequest{
		CommandReference: definition.CommandReference,
		Argv:             definition.Argv,
		TimeoutNanos:     definition.Timeout.Nanoseconds(),
		OutputBytes:      definition.Limits.OutputBytes,
	})
	if err != nil {
		return ExecutionMeasurement{}, fmt.Errorf("encode verification worker request: %w", err)
	}
	name := "harness-verification-" + uuid.NewString()
	user := strconv.Itoa(d.UID) + ":" + strconv.Itoa(d.GID)
	arguments := []string{
		"run", "--rm", "-i", "--name", name, "--network", "none", "--read-only",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--cpus", "1", "--memory", "1g", "--pids-limit", "256", "--user", user,
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=512m,mode=1777",
		"--env", "HOME=/tmp/home", "--env", "TMPDIR=/tmp",
		"--env", "GOCACHE=/tmp/go-build", "--env", "UV_CACHE_DIR=/tmp/uv-cache",
		"--env", "UV_OFFLINE=1", "--env", "UV_NO_SYNC=1",
		"--env", "UV_PROJECT_ENVIRONMENT=/opt/venv",
		"--env", "PYTHONPATH=/workspace/python/src",
		"--env", "PATH=/opt/bin:/opt/venv/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin",
		"--mount", "type=bind,src=" + workspace + ",dst=/workspace",
		"--workdir", "/workspace",
		imageID,
	}
	command := exec.CommandContext(ctx, "docker", arguments...)
	command.Stdin = bytes.NewReader(payload)
	var output boundedBuffer
	output.limit = 2*DefaultMaxOutputBytes + 64*1024
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			removeVerificationContainer(name)
			return ExecutionMeasurement{}, ctx.Err()
		}
		return ExecutionMeasurement{}, fmt.Errorf(
			"verification worker: %w: %s", err, strings.TrimSpace(string(output.Bytes())),
		)
	}
	if output.overflow {
		return ExecutionMeasurement{}, ErrWorkerResponse
	}
	var response WorkerResponse
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return ExecutionMeasurement{}, fmt.Errorf("%w: %v", ErrWorkerResponse, err)
	}
	if response.StartedAt.IsZero() || response.FinishedAt.Before(response.StartedAt) ||
		response.ExitCode < -1 || response.PeakRSSBytes < 0 {
		return ExecutionMeasurement{}, ErrWorkerResponse
	}
	return ExecutionMeasurement{
		StartedAt: response.StartedAt, FinishedAt: response.FinishedAt,
		ExitCode: response.ExitCode, TimedOut: response.TimedOut, Output: response.Output,
		WallTime:     time.Duration(response.WallTimeNanos),
		UserTime:     time.Duration(response.UserTimeNanos),
		SystemTime:   time.Duration(response.SystemTimeNanos),
		PeakRSSBytes: response.PeakRSSBytes, OutputTruncated: response.OutputTruncated,
	}, nil
}

func removeVerificationContainer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()
}

// WorkerRequest is the verification-worker input protocol.
type WorkerRequest struct {
	CommandReference string   `json:"command_reference"`
	Argv             []string `json:"argv"`
	TimeoutNanos     int64    `json:"timeout_nanoseconds"`
	OutputBytes      int64    `json:"output_bytes"`
}

// WorkerResponse is the verification-worker output protocol.
type WorkerResponse struct {
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at"`
	ExitCode        int       `json:"exit_code"`
	TimedOut        bool      `json:"timed_out"`
	Output          []byte    `json:"output"`
	WallTimeNanos   int64     `json:"wall_time_nanoseconds"`
	UserTimeNanos   int64     `json:"user_time_nanoseconds"`
	SystemTimeNanos int64     `json:"system_time_nanoseconds"`
	PeakRSSBytes    int64     `json:"peak_rss_bytes"`
	OutputTruncated bool      `json:"output_truncated"`
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

func (b *boundedBuffer) Bytes() []byte { return b.buffer.Bytes() }
