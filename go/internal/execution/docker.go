package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/dockerengine"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"github.com/google/uuid"
)

type DockerApplicator struct {
	Image  string
	UID    int
	GID    int
	Engine dockerengine.Engine
}

func (d DockerApplicator) Apply(
	ctx context.Context, worktree string, changes []contracts.FileChange, limits Limits,
) error {
	payload, err := json.Marshal(workerRequest{Changes: changes, Limits: limits})
	if err != nil {
		return fmt.Errorf("encode worker request: %w", err)
	}
	user := strconv.Itoa(d.UID) + ":" + strconv.Itoa(d.GID)
	containerName := "harness-execution-" + uuid.NewString()
	engine, closeEngine, err := d.engine()
	if err != nil {
		return err
	}
	defer closeEngine()
	var output bytes.Buffer
	err = engine.Run(ctx, dockerengine.RunRequest{
		Name: containerName, Image: d.Image, User: user,
		Mounts: []dockerengine.Mount{
			{Source: worktree, Target: "/workspace"},
			{Source: worktree + "/.git", Target: "/workspace/.git", ReadOnly: true},
		},
		Tmpfs:  map[string]string{"/tmp": "rw,noexec,nosuid,nodev,size=16m"},
		Memory: 512 << 20, Pids: 64,
	}, bytes.NewReader(payload), &output)
	if err != nil {
		if ctx.Err() != nil {
			removeContainer(engine, containerName)
			return fmt.Errorf("execution worker: %w", ctx.Err())
		}
		return fmt.Errorf("execution worker: %w: %s", err, output.String())
	}
	return nil
}

func (d DockerApplicator) engine() (dockerengine.Engine, func(), error) {
	if d.Engine != nil {
		return d.Engine, func() {}, nil
	}
	client, err := dockerengine.NewFromEnvironment()
	if err != nil {
		return nil, func() {}, err
	}
	return client, func() { _ = client.Close() }, nil
}

func removeContainer(engine dockerengine.Engine, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = engine.Remove(ctx, name)
}

var _ io.Writer = (*bytes.Buffer)(nil)

type workerRequest struct {
	Changes []contracts.FileChange `json:"changes"`
	Limits  Limits                 `json:"limits"`
}
