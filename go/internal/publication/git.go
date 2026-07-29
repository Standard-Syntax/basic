package publication

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var (
	commitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
	branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

type GitCommandPublisher struct {
	root       string
	remote     string
	baseBranch string
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
	command.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0=/dev/null",
		"GIT_CONFIG_KEY_1=credential.interactive",
		"GIT_CONFIG_VALUE_1=never",
	)
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
