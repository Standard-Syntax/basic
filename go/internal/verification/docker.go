package verification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/dockerengine"
	"github.com/google/uuid"
)

var imageIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type DockerCheckExecutor struct {
	Image  string
	UID    int
	GID    int
	Engine dockerengine.Engine
}

func (d DockerCheckExecutor) ImageID(ctx context.Context) (string, error) {
	if d.Image == "" || d.UID <= 0 || d.GID <= 0 {
		return "", errors.New("verification image and non-root UID/GID are required")
	}
	engine, closeEngine, err := d.engine()
	if err != nil {
		return "", err
	}
	defer closeEngine()
	imageID, err := engine.ImageID(ctx, d.Image)
	if err != nil {
		return "", fmt.Errorf("inspect verification image: %w", err)
	}
	if !imageIDPattern.MatchString(imageID) {
		return "", fmt.Errorf("%w: image identity", ErrWorkerResponse)
	}
	return imageID, nil
}

func (d DockerCheckExecutor) Run(
	ctx context.Context, workspace, imageID string, definition CheckDefinition,
) (ExecutionMeasurement, error) {
	if !imageIDPattern.MatchString(imageID) {
		return ExecutionMeasurement{}, fmt.Errorf("%w: image identity", ErrWorkerResponse)
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
	output := dockerengine.NewBoundedWriter(2*DefaultMaxOutputBytes + 64*1024)
	engine, closeEngine, err := d.engine()
	if err != nil {
		return ExecutionMeasurement{}, err
	}
	defer closeEngine()
	runCtx, cancelRun := context.WithTimeout(ctx, definition.Timeout+15*time.Second)
	defer cancelRun()
	err = engine.Run(runCtx, dockerengine.RunRequest{
		Name: name, Image: imageID, User: user, WorkingDir: "/workspace",
		Env: []string{"HOME=/tmp/home", "TMPDIR=/tmp", "GOCACHE=/tmp/go-build",
			"UV_CACHE_DIR=/tmp/uv-cache", "UV_OFFLINE=1", "UV_NO_SYNC=1",
			"UV_PROJECT_ENVIRONMENT=/opt/venv", "PYTHONPATH=/workspace/python/src",
			"PATH=/opt/bin:/opt/venv/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"},
		Mounts:   []dockerengine.Mount{{Source: workspace, Target: "/workspace"}},
		Tmpfs:    map[string]string{"/tmp": "rw,exec,nosuid,nodev,size=2g,mode=1777"},
		NanoCPUs: int64(definition.Limits.CPUs) * 1_000_000_000,
		Memory:   definition.Limits.MemoryBytes, Pids: int64(definition.Limits.PIDs),
	}, bytes.NewReader(payload), output)
	if err != nil {
		if runCtx.Err() != nil {
			removeVerificationContainer(engine, name)
			if ctx.Err() != nil {
				return ExecutionMeasurement{}, ctx.Err()
			}
			return ExecutionMeasurement{}, fmt.Errorf("verification worker: %w", runCtx.Err())
		}
		return ExecutionMeasurement{}, fmt.Errorf(
			"verification worker: %w: %s", err, strings.TrimSpace(string(output.Bytes())),
		)
	}
	if output.Overflow() {
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

func (d DockerCheckExecutor) engine() (dockerengine.Engine, func(), error) {
	if d.Engine != nil {
		return d.Engine, func() {}, nil
	}
	client, err := dockerengine.NewFromEnvironment()
	if err != nil {
		return nil, func() {}, err
	}
	return client, func() { _ = client.Close() }, nil
}

func removeVerificationContainer(engine dockerengine.Engine, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = engine.Remove(ctx, name)
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
