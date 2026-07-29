package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	if err := materializeTree(ctx, config.RepositoryRoot, worktree, baseCommit); err != nil {
		_ = removeWorktree(context.Background(), config.RepositoryRoot, worktree)
		return "", err
	}
	return worktree, nil
}

func materializeTree(ctx context.Context, repository, worktree, commit string) error {
	tree, err := gitOutput(ctx, repository, "ls-tree", "-rz", "--full-tree", "-r", commit)
	if err != nil {
		return fmt.Errorf("list base tree: %w", err)
	}
	root, err := os.OpenRoot(worktree)
	if err != nil {
		return fmt.Errorf("open worktree root: %w", err)
	}
	defer root.Close()
	for _, record := range bytes.Split(tree, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		header, pathBytes, ok := bytes.Cut(record, []byte{'\t'})
		fields := bytes.Fields(header)
		if !ok || len(fields) != 3 {
			return errors.New("invalid git tree record")
		}
		mode, objectType, objectID := string(fields[0]), string(fields[1]), string(fields[2])
		path := string(pathBytes)
		if _, err := normalizePath(path); err != nil {
			return fmt.Errorf("%w: tracked path %q", ErrUnsafePath, path)
		}
		if objectType != "blob" || (mode != "100644" && mode != "100755" && mode != "120000") {
			return fmt.Errorf("%w: unsupported tracked entry %q mode %s", ErrUnsafePath, path, mode)
		}
		body, err := gitOutput(ctx, repository, "cat-file", "blob", objectID)
		if err != nil {
			return fmt.Errorf("read tracked blob %q: %w", path, err)
		}
		parent := filepath.ToSlash(filepath.Dir(path))
		if parent != "." {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("create tracked parent %q: %w", parent, err)
			}
		}
		if mode == "120000" {
			if err := root.Symlink(string(body), path); err != nil {
				return fmt.Errorf("materialize symlink %q: %w", path, err)
			}
			continue
		}
		permissions, _ := strconv.ParseUint(mode[3:], 8, 32)
		if err := root.WriteFile(path, body, os.FileMode(permissions)); err != nil {
			return fmt.Errorf("materialize blob %q: %w", path, err)
		}
	}
	return nil
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
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", arguments[0], err, strings.TrimSpace(string(output)))
	}
	return output, nil
}
