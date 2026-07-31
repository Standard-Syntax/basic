// Package beta defines the immutable production-beta repository policy.
package beta

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const PolicyVersion = "1.0"

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	imagePattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type Repository struct {
	Owner      string `json:"owner"`
	Name       string `json:"name"`
	Root       string `json:"root"`
	Remote     string `json:"remote"`
	RemoteURL  string `json:"remote_url"`
	BaseBranch string `json:"base_branch"`
	BaseCommit string `json:"base_commit"`
}

type Paths struct {
	Readable   []string `json:"readable"`
	Writable   []string `json:"writable"`
	Prohibited []string `json:"prohibited"`
}

type Limits struct {
	MaximumTasks            int   `json:"maximum_tasks"`
	MaximumChangedFiles     int   `json:"maximum_changed_files"`
	MaximumFileBytes        int64 `json:"maximum_file_bytes"`
	MaximumTotalBytes       int64 `json:"maximum_total_bytes"`
	ExecutionConcurrency    int   `json:"execution_concurrency"`
	VerificationConcurrency int   `json:"verification_concurrency"`
}

type Images struct {
	Execution    string `json:"execution"`
	Verification string `json:"verification"`
}

type Policy struct {
	Version       string     `json:"version"`
	Repository    Repository `json:"repository"`
	Paths         Paths      `json:"paths"`
	TrustedChecks []string   `json:"trusted_checks"`
	Limits        Limits     `json:"limits"`
	Images        Images     `json:"images"`
}

func DecodePolicy(body []byte) (Policy, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var value Policy
	if err := decoder.Decode(&value); err != nil {
		return Policy{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Policy{}, errors.New("policy has trailing content")
	}
	if err := value.Validate(); err != nil {
		return Policy{}, err
	}
	return value, nil
}

func (p Policy) Canonical() ([]byte, string, error) {
	if err := p.Validate(); err != nil {
		return nil, "", err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(body)
	return body, hex.EncodeToString(digest[:]), nil
}

func (p Policy) Validate() error {
	if p.Version != PolicyVersion {
		return errors.New("unsupported beta policy version")
	}
	r := p.Repository
	if r.Owner == "" || r.Name == "" || r.Remote == "" || r.RemoteURL == "" ||
		r.BaseBranch == "" || !cleanAbsolute(r.Root) || !commitPattern.MatchString(r.BaseCommit) {
		return errors.New("invalid repository policy")
	}
	if !imagePattern.MatchString(p.Images.Execution) || !imagePattern.MatchString(p.Images.Verification) {
		return errors.New("worker images must be immutable sha256 identities")
	}
	if p.Limits.MaximumTasks != 1 || p.Limits.MaximumChangedFiles < 1 ||
		p.Limits.MaximumFileBytes < 1 || p.Limits.MaximumTotalBytes < p.Limits.MaximumFileBytes ||
		p.Limits.ExecutionConcurrency < 1 || p.Limits.VerificationConcurrency < 1 ||
		p.Limits.MaximumChangedFiles > 100 || p.Limits.MaximumFileBytes > 1<<20 ||
		p.Limits.MaximumTotalBytes > 10<<20 || p.Limits.ExecutionConcurrency > 4 ||
		p.Limits.VerificationConcurrency > 2 {
		return errors.New("invalid beta limits")
	}
	if err := validatePathPolicy(p.Paths); err != nil {
		return err
	}
	if len(p.TrustedChecks) == 0 {
		return errors.New("trusted checks are required")
	}
	seen := map[string]bool{}
	for _, check := range p.TrustedChecks {
		if check == "" || strings.TrimSpace(check) != check || seen[check] {
			return errors.New("trusted checks must be unique and non-empty")
		}
		seen[check] = true
	}
	if !slices.IsSorted(p.TrustedChecks) {
		return errors.New("trusted checks must be sorted")
	}
	return nil
}

func validatePathPolicy(value Paths) error {
	for _, group := range [][]string{value.Readable, value.Writable, value.Prohibited} {
		if len(group) == 0 || !slices.IsSorted(group) {
			return errors.New("policy paths must be non-empty and sorted")
		}
		previous := ""
		for _, item := range group {
			if !safePath(item) || item == previous {
				return errors.New("invalid or duplicate policy path")
			}
			previous = item
		}
	}
	for _, writable := range value.Writable {
		if !WithinAny(writable, value.Readable) || WithinAny(writable, value.Prohibited) {
			return errors.New("writable paths must be readable and not prohibited")
		}
	}
	for _, prohibited := range value.Prohibited {
		if WithinAny(prohibited, value.Writable) {
			return errors.New("prohibited and writable paths overlap")
		}
	}
	return nil
}

func safePath(value string) bool {
	return value != "" && value != "." && !strings.HasPrefix(value, "/") &&
		path.Clean(value) == value && !strings.HasPrefix(value, "../") && !strings.ContainsRune(value, '\x00')
}

func WithinAny(value string, roots []string) bool {
	for _, root := range roots {
		if value == root || strings.HasPrefix(value, root+"/") {
			return true
		}
	}
	return false
}

func cleanAbsolute(value string) bool { return filepath.IsAbs(value) && filepath.Clean(value) == value }

// VerifyRemoteBase rechecks the configured remote and approved branch without changing refs.
func VerifyRemoteBase(ctx context.Context, p Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	configured, err := git(ctx, p.Repository.Root, "remote", "get-url", p.Repository.Remote)
	if err != nil || configured != p.Repository.RemoteURL {
		return errors.New("repository remote mismatch")
	}
	head, err := git(ctx, p.Repository.Root, "ls-remote", "--heads", p.Repository.Remote, "refs/heads/"+p.Repository.BaseBranch)
	if err != nil {
		return fmt.Errorf("read remote base: %w", err)
	}
	fields := strings.Fields(head)
	if len(fields) != 2 || fields[0] != p.Repository.BaseCommit {
		return errors.New("approved remote base moved")
	}
	resolved, err := git(ctx, p.Repository.Root, "rev-parse", "--verify", p.Repository.BaseCommit+"^{commit}")
	if err != nil || resolved != p.Repository.BaseCommit {
		return errors.New("approved base is not a local commit")
	}
	localHead, err := git(ctx, p.Repository.Root, "rev-parse", "--verify", "refs/heads/"+p.Repository.BaseBranch)
	if err != nil || localHead != p.Repository.BaseCommit {
		return errors.New("local base branch does not equal approved commit")
	}
	return nil
}

func git(ctx context.Context, root string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	command.Env = append(command.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(output)), nil
}
