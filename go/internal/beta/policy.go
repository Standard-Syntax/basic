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
	"regexp"
	"slices"
	"strings"

	"github.com/Standard-Syntax/basic/go/internal/subprocess"
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
	if err := validateRepository(p.Repository); err != nil {
		return err
	}
	if err := validateImages(p.Images); err != nil {
		return err
	}
	if err := validateLimits(p.Limits); err != nil {
		return err
	}
	if err := validatePathPolicy(p.Paths); err != nil {
		return err
	}
	return validateTrustedChecks(p.TrustedChecks)
}

func validateRepository(value Repository) error {
	if value.Owner == "" || value.Name == "" || value.Remote == "" || value.RemoteURL == "" ||
		value.BaseBranch == "" || !CleanAbsolute(value.Root) || !commitPattern.MatchString(value.BaseCommit) {
		return errors.New("invalid repository policy")
	}
	return nil
}

func validateImages(value Images) error {
	if !imagePattern.MatchString(value.Execution) || !imagePattern.MatchString(value.Verification) {
		return errors.New("worker images must be immutable sha256 identities")
	}
	return nil
}

func validateLimits(value Limits) error {
	if value.MaximumTasks != 1 || value.MaximumChangedFiles < 1 ||
		value.MaximumFileBytes < 1 || value.MaximumTotalBytes < value.MaximumFileBytes ||
		value.ExecutionConcurrency < 1 || value.VerificationConcurrency < 1 ||
		value.MaximumChangedFiles > 100 || value.MaximumFileBytes > 1<<20 ||
		value.MaximumTotalBytes > 10<<20 || value.ExecutionConcurrency > 4 ||
		value.VerificationConcurrency > 2 {
		return errors.New("invalid beta limits")
	}
	return nil
}

func validateTrustedChecks(values []string) error {
	if len(values) == 0 {
		return errors.New("trusted checks are required")
	}
	seen := map[string]bool{}
	for _, check := range values {
		if check == "" || strings.TrimSpace(check) != check || seen[check] {
			return errors.New("trusted checks must be unique and non-empty")
		}
		seen[check] = true
	}
	if !slices.IsSorted(values) {
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
	return value != "" && value != "." && value != ".." && !strings.HasPrefix(value, "/") &&
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

// VerifyRemoteBase rechecks the configured remote and approved branch without changing refs.
func VerifyRemoteBase(ctx context.Context, p Policy) error {
	return verifyRemoteBaseUsingCredential(ctx, p, "")
}

// VerifyRemoteBaseWithSSHKey performs the same non-mutating verification while
// binding remote access to the separately provisioned canary deploy key.
func VerifyRemoteBaseWithSSHKey(ctx context.Context, p Policy, sshKeyFile string) error {
	if !CleanAbsolute(sshKeyFile) || !canaryCredentialPathPattern.MatchString(sshKeyFile) {
		return errors.New("invalid Git SSH credential path")
	}
	return verifyRemoteBaseUsingCredential(ctx, p, sshKeyFile)
}

func verifyRemoteBaseUsingCredential(ctx context.Context, p Policy, sshKeyFile string) error {
	if err := p.Validate(); err != nil {
		return err
	}
	configured, err := git(ctx, p.Repository.Root, sshKeyFile, "remote", "get-url", p.Repository.Remote)
	if err != nil || configured != p.Repository.RemoteURL {
		return errors.New("repository remote mismatch")
	}
	head, err := git(ctx, p.Repository.Root, sshKeyFile, "ls-remote", "--heads", p.Repository.Remote, "refs/heads/"+p.Repository.BaseBranch)
	if err != nil {
		return fmt.Errorf("read remote base: %w", err)
	}
	fields := strings.Fields(head)
	if len(fields) != 2 || fields[0] != p.Repository.BaseCommit {
		return errors.New("approved remote base moved")
	}
	resolved, err := git(ctx, p.Repository.Root, sshKeyFile, "rev-parse", "--verify", p.Repository.BaseCommit+"^{commit}")
	if err != nil || resolved != p.Repository.BaseCommit {
		return errors.New("approved base is not a local commit")
	}
	localHead, err := git(ctx, p.Repository.Root, sshKeyFile, "rev-parse", "--verify", "refs/heads/"+p.Repository.BaseBranch)
	if err != nil || localHead != p.Repository.BaseCommit {
		return errors.New("local base branch does not equal approved commit")
	}
	return nil
}

func git(ctx context.Context, root, sshKeyFile string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	environment, err := subprocess.RemoteGit(sshKeyFile)
	if err != nil {
		return "", err
	}
	command.Env = environment
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(output)), nil
}
