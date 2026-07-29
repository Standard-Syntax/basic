package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
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
	arguments := []string{
		"run", "--rm", "--network", "none", "--read-only",
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
		return fmt.Errorf("execution worker: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

type workerRequest struct {
	Changes []contracts.FileChange `json:"changes"`
	Limits  Limits                 `json:"limits"`
}
