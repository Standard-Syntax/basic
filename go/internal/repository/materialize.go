// Package repository provides hook- and filter-free access to Git objects.
package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	ErrInvalidCommit = errors.New("invalid git commit")
	ErrUnsafePath    = errors.New("unsafe repository path")
)

// MaterializeTree writes the tracked blobs in commit directly from Git's object
// database. It never invokes checkout, attributes, filters, hooks, textconv, or
// external diff drivers.
func MaterializeTree(ctx context.Context, repository, destination, commit string) error {
	objectType, err := gitOutput(ctx, repository, "cat-file", "-t", commit)
	if err != nil || strings.TrimSpace(string(objectType)) != "commit" {
		return ErrInvalidCommit
	}
	tree, err := gitOutput(ctx, repository, "ls-tree", "-rz", "--full-tree", "-r", commit)
	if err != nil {
		return fmt.Errorf("list tree: %w", err)
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return fmt.Errorf("open materialization root: %w", err)
	}
	defer root.Close()
	for _, record := range bytes.Split(tree, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		mode, objectID, name, err := parseTreeRecord(record)
		if err != nil {
			return err
		}
		if err := materializeEntry(ctx, repository, root, mode, objectID, name); err != nil {
			return err
		}
	}
	return nil
}

func parseTreeRecord(record []byte) (string, string, string, error) {
	header, pathBytes, ok := bytes.Cut(record, []byte{'\t'})
	fields := bytes.Fields(header)
	if !ok || len(fields) != 3 {
		return "", "", "", errors.New("invalid git tree record")
	}
	mode, objectType, objectID := string(fields[0]), string(fields[1]), string(fields[2])
	name := string(pathBytes)
	if !safePath(name) {
		return "", "", "", fmt.Errorf("%w: tracked path %q", ErrUnsafePath, name)
	}
	if objectType != "blob" || (mode != "100644" && mode != "100755" && mode != "120000") {
		return "", "", "", fmt.Errorf(
			"%w: unsupported tracked entry %q mode %s", ErrUnsafePath, name, mode,
		)
	}
	return mode, objectID, name, nil
}

func safePath(name string) bool {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name ||
		strings.Contains(name, `\`) {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || component == "." || component == ".." || component == ".git" {
			return false
		}
	}
	return true
}

func materializeEntry(
	ctx context.Context, repository string, root *os.Root, mode, objectID, name string,
) error {
	body, err := gitOutput(ctx, repository, "cat-file", "blob", objectID)
	if err != nil {
		return fmt.Errorf("read tracked blob %q: %w", name, err)
	}
	parent := filepath.ToSlash(filepath.Dir(name))
	if parent != "." {
		if err := root.MkdirAll(parent, 0o750); err != nil {
			return fmt.Errorf("create tracked parent %q: %w", parent, err)
		}
	}
	if mode == "120000" {
		if err := root.Symlink(string(body), name); err != nil {
			return fmt.Errorf("materialize symlink %q: %w", name, err)
		}
		return nil
	}
	permissions, _ := strconv.ParseUint(mode[3:], 8, 32)
	if err := root.WriteFile(name, body, os.FileMode(permissions)); err != nil {
		return fmt.Errorf("materialize blob %q: %w", name, err)
	}
	return nil
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
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_OPTIONAL_LOCKS=0",
		"LANG=C",
	}
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
