package publication

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/Standard-Syntax/basic/go/internal/subprocess"
)

var (
	commitPattern         = regexp.MustCompile(`^[a-f0-9]{40}$`)
	branchPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	credentialPathPattern = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
)

type GitCommandPublisher struct {
	root       string
	remote     string
	baseBranch string
	sshKeyFile string
}

// NewAuthenticatedGitCommandPublisher binds Git pushes to one explicitly
// mounted owner-only SSH key. The legacy constructor remains for loopback and
// non-canary callers that provide authentication outside this process.
func NewAuthenticatedGitCommandPublisher(
	root, remote, baseBranch, sshKeyFile string,
) (*GitCommandPublisher, error) {
	publisher, err := NewGitCommandPublisher(root, remote, baseBranch)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(sshKeyFile) || filepath.Clean(sshKeyFile) != sshKeyFile ||
		!credentialPathPattern.MatchString(sshKeyFile) {
		return nil, ErrCredentialPermissions
	}
	info, err := os.Lstat(sshKeyFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm() != 0o600 {
		return nil, ErrCredentialPermissions
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return nil, ErrCredentialPermissions
	}
	publisher.sshKeyFile = sshKeyFile
	return publisher, nil
}

func NewGitCommandPublisher(root, remote, baseBranch string) (*GitCommandPublisher, error) {
	if strings.TrimSpace(root) == "" || !branchPattern.MatchString(remote) ||
		!branchPattern.MatchString(baseBranch) {
		return nil, fmt.Errorf("%w: git configuration", ErrInvalidRequest)
	}
	return &GitCommandPublisher{root: root, remote: remote, baseBranch: baseBranch}, nil
}

func (p *GitCommandPublisher) BaseHead(ctx context.Context) (string, error) {
	head, exists, err := p.remoteHead(ctx, p.baseBranch)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("%w: remote base branch is missing", ErrBaseDrift)
	}
	return head, nil
}

func (p *GitCommandPublisher) BranchHead(
	ctx context.Context, branch string,
) (string, bool, error) {
	if !branchPattern.MatchString(branch) {
		return "", false, fmt.Errorf("%w: branch name", ErrInvalidRequest)
	}
	return p.remoteHead(ctx, branch)
}

func (p *GitCommandPublisher) Publish(
	ctx context.Context, branch, candidate string,
) (bool, error) {
	if !branchPattern.MatchString(branch) || !commitPattern.MatchString(candidate) {
		return false, fmt.Errorf("%w: branch publication binding", ErrInvalidRequest)
	}
	if err := p.run(ctx, nil, "cat-file", "-e", candidate+"^{commit}"); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, fmt.Errorf("%w: candidate commit is unavailable: %v", ErrInvalidRequest, err)
	}
	head, exists, err := p.BranchHead(ctx, branch)
	if err != nil {
		return false, err
	}
	if exists {
		if head == candidate {
			return true, nil
		}
		return false, ErrBranchConflict
	}
	ref := "refs/heads/" + branch
	pushErr := p.run(ctx, nil, "push", "--porcelain", "--no-verify",
		"--force-with-lease="+ref+":", p.remote, candidate+":"+ref)
	if pushErr == nil {
		return false, nil
	}
	head, exists, inspectErr := p.BranchHead(ctx, branch)
	if inspectErr == nil && exists && head == candidate {
		return true, nil
	}
	if inspectErr == nil && exists {
		return false, ErrBranchConflict
	}
	return false, fmt.Errorf("publish candidate branch: %w", pushErr)
}

// DeleteBranch removes only the exact branch if its remote head still equals
// candidate. A missing branch is an idempotent success.
func (p *GitCommandPublisher) DeleteBranch(
	ctx context.Context, branch, candidate string,
) (bool, error) {
	if !branchPattern.MatchString(branch) || !commitPattern.MatchString(candidate) {
		return false, fmt.Errorf("%w: branch cleanup binding", ErrInvalidRequest)
	}
	head, exists, err := p.BranchHead(ctx, branch)
	if err != nil {
		return false, err
	}
	if !exists {
		return true, nil
	}
	if head != candidate {
		return false, ErrBranchConflict
	}
	ref := "refs/heads/" + branch
	deleteErr := p.run(ctx, nil, "push", "--porcelain", "--no-verify",
		"--force-with-lease="+ref+":"+candidate, p.remote, ":"+ref)
	if deleteErr == nil {
		return false, nil
	}
	head, exists, inspectErr := p.BranchHead(ctx, branch)
	if inspectErr == nil && !exists {
		return true, nil
	}
	if inspectErr == nil && head != candidate {
		return false, ErrBranchConflict
	}
	return false, fmt.Errorf("delete publication branch: %w", deleteErr)
}

func (p *GitCommandPublisher) remoteHead(
	ctx context.Context, branch string,
) (string, bool, error) {
	var output bytes.Buffer
	err := p.run(ctx, &output, "ls-remote", "--heads", p.remote, "refs/heads/"+branch)
	if err != nil {
		return "", false, fmt.Errorf("inspect remote branch: %w", err)
	}
	fields := strings.Fields(output.String())
	if len(fields) == 0 {
		return "", false, nil
	}
	if len(fields) != 2 || !commitPattern.MatchString(fields[0]) {
		return "", false, errors.New("invalid git ls-remote response")
	}
	return fields[0], true, nil
}

func (p *GitCommandPublisher) run(
	ctx context.Context, stdout *bytes.Buffer, args ...string,
) error {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = p.root
	environment, err := subprocess.RemoteGit(p.sshKeyFile)
	if err != nil {
		return ErrCredentialPermissions
	}
	command.Env = environment
	var stderr bytes.Buffer
	if stdout != nil {
		command.Stdout = stdout
	}
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, message)
	}
	return nil
}
