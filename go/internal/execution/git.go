package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	repositoryutil "github.com/Standard-Syntax/basic/go/internal/repository"
)

func createWorktree(
	ctx context.Context, config Config, executionID, baseCommit string,
) (string, error) {
	if err := os.MkdirAll(config.WorktreeRoot, 0o700); err != nil {
		return "", fmt.Errorf("create worktree root: %w", err)
	}
	worktree := filepath.Join(config.WorktreeRoot, executionID)
	if err := removeWorktree(ctx, config.RepositoryRoot, worktree); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	objectType, err := gitOutput(ctx, config.RepositoryRoot, "cat-file", "-t", baseCommit)
	if err != nil || strings.TrimSpace(string(objectType)) != "commit" {
		return "", fmt.Errorf("%w: base commit", ErrInvalidRequest)
	}
	if _, err := gitOutput(
		ctx, config.RepositoryRoot, "worktree", "add", "--detach", "--no-checkout",
		worktree, baseCommit,
	); err != nil {
		return "", fmt.Errorf("create detached worktree: %w", err)
	}
	if err := repositoryutil.MaterializeTree(
		ctx, config.RepositoryRoot, worktree, baseCommit,
	); err != nil {
		_ = removeWorktree(context.Background(), config.RepositoryRoot, worktree)
		if errors.Is(err, repositoryutil.ErrUnsafePath) {
			return "", fmt.Errorf("%w: %w", ErrUnsafePath, err)
		}
		return "", err
	}
	return worktree, nil
}

func removeWorktree(ctx context.Context, repository, worktree string) error {
	if _, err := os.Lstat(worktree); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_, gitErr := gitOutput(ctx, repository, "worktree", "remove", "--force", worktree)
	if gitErr == nil {
		return nil
	}
	return fmt.Errorf("remove worktree: %w", gitErr)
}

func gitOutput(ctx context.Context, repository string, arguments ...string) ([]byte, error) {
	base := []string{
		"--literal-pathspecs",
		"-c", "core.hooksPath=/dev/null",
		"-c", "diff.external=",
		"-c", "core.attributesFile=/dev/null",
		"-C", repository,
	}
	command := exec.CommandContext(ctx, "git", append(base, arguments...)...)
	command.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_OPTIONAL_LOCKS=0",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf(
			"git %s: %w: %s", arguments[0], err, strings.TrimSpace(stderr.String()),
		)
	}
	return stdout.Bytes(), nil
}
