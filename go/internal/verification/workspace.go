package verification

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	repositoryutil "github.com/Standard-Syntax/basic/go/internal/repository"
)

type FileWorkspacePreparer struct {
	RepositoryRoot string
	WorkspaceRoot  string
}

func (p FileWorkspacePreparer) Prepare(
	ctx context.Context, verificationID, candidateCommit string,
) (string, func() error, error) {
	if p.RepositoryRoot == "" || p.WorkspaceRoot == "" {
		return "", nil, errors.New("repository and workspace roots are required")
	}
	if err := os.MkdirAll(p.WorkspaceRoot, 0o700); err != nil {
		return "", nil, fmt.Errorf("create verification workspace root: %w", err)
	}
	workspace, err := os.MkdirTemp(p.WorkspaceRoot, verificationID+"-")
	if err != nil {
		return "", nil, fmt.Errorf("create verification workspace: %w", err)
	}
	cleanup := func() error {
		if err := os.RemoveAll(workspace); err != nil {
			return fmt.Errorf("remove verification workspace: %w", err)
		}
		return nil
	}
	if err := repositoryutil.MaterializeTree(
		ctx, p.RepositoryRoot, workspace, candidateCommit,
	); err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("materialize candidate commit: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(p.WorkspaceRoot)
	if err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("resolve verification workspace root: %w", err)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil || filepath.Dir(resolvedWorkspace) != resolvedRoot {
		_ = cleanup()
		return "", nil, errors.New("verification workspace escaped configured root")
	}
	return workspace, cleanup, nil
}
