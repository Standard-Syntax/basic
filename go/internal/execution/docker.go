package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"github.com/google/uuid"
)

type DockerApplicator struct {
	Image string
	UID   int
	GID   int
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
	arguments := []string{
		"run", "--rm", "-i", "--name", containerName, "--network", "none", "--read-only",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--cpus", "1", "--memory", "512m", "--pids-limit", "64",
		"--user", user, "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16m",
		"--mount", "type=bind,src=" + worktree + ",dst=/workspace",
		"--mount", "type=bind,src=" + worktree + "/.git,dst=/workspace/.git,readonly",
		d.Image,
	}
	command := exec.CommandContext(ctx, "docker", arguments...)
	command.Stdin = bytes.NewReader(payload)
	output, err := command.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			removeContainer(containerName)
			return fmt.Errorf("execution worker: %w", ctx.Err())
		}
		return fmt.Errorf("execution worker: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func removeContainer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()
}

type workerRequest struct {
	Changes []contracts.FileChange `json:"changes"`
	Limits  Limits                 `json:"limits"`
}
