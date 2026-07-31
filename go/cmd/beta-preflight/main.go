// Command beta-preflight performs non-mutating production readiness checks.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/approval"
	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/migration"
	"github.com/Standard-Syntax/basic/go/internal/publication"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
	"github.com/Standard-Syntax/basic/go/internal/registry"
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
)

type config struct {
	DatabaseURL                   string      `json:"database_url"`
	ArtifactRoot                  string      `json:"artifact_root"`
	WorktreeRoot                  string      `json:"worktree_root"`
	VerificationWorkspaceRoot     string      `json:"verification_workspace_root"`
	ProviderCredentialEnvironment string      `json:"provider_credential_environment"`
	PublicationCredentialFile     string      `json:"publication_credential_file"`
	Policy                        beta.Policy `json:"beta_policy"`
}

type report struct {
	Status            string   `json:"status"`
	RepositoryOwner   string   `json:"repository_owner,omitempty"`
	RepositoryName    string   `json:"repository_name,omitempty"`
	BaseCommit        string   `json:"base_commit,omitempty"`
	PolicyDigest      string   `json:"policy_digest,omitempty"`
	ExecutionImage    string   `json:"execution_image,omitempty"`
	VerificationImage string   `json:"verification_image,omitempty"`
	MigrationDigest   string   `json:"migration_digest,omitempty"`
	FailureCodes      []string `json:"failure_codes"`
}

func main() { os.Exit(mainExit()) }

func mainExit() int {
	path := flag.String("config", "", "absolute path to strict beta configuration")
	flag.Parse()
	value, err := loadConfig(*path)
	if err != nil {
		emit(report{Status: "invalid", FailureCodes: []string{"CONFIG_INVALID"}})
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	result := check(ctx, value)
	emit(result)
	if result.Status != "ready" {
		return 1
	}
	return 0
}

func loadConfig(name string) (config, error) {
	if !cleanAbsolute(name) {
		return config{}, errors.New("config path must be clean and absolute")
	}
	file, err := os.Open(name)
	if err != nil {
		return config{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var value config
	if err := decoder.Decode(&value); err != nil {
		return config{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return config{}, errors.New("trailing configuration")
	}
	if value.DatabaseURL == "" || value.ProviderCredentialEnvironment == "" ||
		!cleanAbsolute(value.ArtifactRoot) || !cleanAbsolute(value.WorktreeRoot) ||
		!cleanAbsolute(value.VerificationWorkspaceRoot) || !cleanAbsolute(value.PublicationCredentialFile) {
		return config{}, errors.New("incomplete preflight configuration")
	}
	if err := value.Policy.Validate(); err != nil {
		return config{}, err
	}
	return value, nil
}

func check(ctx context.Context, value config) report {
	_, policyDigest, _ := value.Policy.Canonical()
	result := report{Status: "ready", RepositoryOwner: value.Policy.Repository.Owner,
		RepositoryName: value.Policy.Repository.Name, BaseCommit: value.Policy.Repository.BaseCommit,
		PolicyDigest: policyDigest, ExecutionImage: value.Policy.Images.Execution,
		VerificationImage: value.Policy.Images.Verification, FailureCodes: []string{}}
	failures := map[string]bool{}
	paths := []string{value.Policy.Repository.Root, value.ArtifactRoot, value.WorktreeRoot, value.VerificationWorkspaceRoot}
	if err := checkRoots(paths); err != nil {
		failures["ROOT_UNSAFE"] = true
	}
	if err := checkGit(ctx, value.Policy, value.WorktreeRoot); err != nil {
		failures["REPOSITORY_MISMATCH"] = true
	}
	if err := checkDocker(ctx, value.Policy.Images.Execution); err != nil {
		failures["EXECUTION_IMAGE_MISMATCH"] = true
	}
	if err := checkDocker(ctx, value.Policy.Images.Verification); err != nil {
		failures["VERIFICATION_IMAGE_MISMATCH"] = true
	}
	expected, err := migration.Verify(ctx, value.DatabaseURL, migrationSources()...)
	if err != nil {
		failures["MIGRATION_MISMATCH"] = true
	} else {
		result.MigrationDigest = digestMigrations(expected)
	}
	if credential := os.Getenv(value.ProviderCredentialEnvironment); credential == "" {
		failures["PROVIDER_CREDENTIAL_UNAVAILABLE"] = true
	}
	if err := checkCredentialFile(value.PublicationCredentialFile); err != nil {
		failures["PUBLICATION_CREDENTIAL_INVALID"] = true
	}
	for code := range failures {
		result.FailureCodes = append(result.FailureCodes, code)
	}
	slices.Sort(result.FailureCodes)
	if len(result.FailureCodes) != 0 {
		result.Status = "not_ready"
	}
	return result
}

func checkRoots(paths []string) error {
	for index, root := range paths {
		if err := checkRoot(root); err != nil {
			return err
		}
		for _, other := range paths[:index] {
			if rootsOverlap(root, other) {
				return errors.New("overlapping roots")
			}
		}
	}
	return nil
}

func checkRoot(root string) error {
	info, err := rootInfoWithoutSymlinks(root)
	if err != nil || !info.IsDir() {
		return errors.New("unsafe root")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("unsafe permissions")
	}
	return nil
}

func rootInfoWithoutSymlinks(root string) (os.FileInfo, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("root must be clean and absolute")
	}
	volume := filepath.VolumeName(root)
	current := volume + string(filepath.Separator)
	info, err := os.Lstat(current)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("unsafe root component")
	}
	for _, component := range strings.Split(strings.TrimPrefix(root, current), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err = os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("unsafe root component")
		}
	}
	return info, nil
}

func rootsOverlap(left, right string) bool {
	return beneathOrEqual(left, right) || beneathOrEqual(right, left)
}

func beneathOrEqual(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && (relative == "." ||
		(relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

func checkGit(ctx context.Context, policy beta.Policy, worktreeRoot string) error {
	root, err := command(ctx, "git", "-C", policy.Repository.Root, "rev-parse", "--show-toplevel")
	if err != nil || root != policy.Repository.Root {
		return errors.New("git root mismatch")
	}
	if err := beta.VerifyRemoteBase(ctx, policy); err != nil {
		return err
	}
	status, err := command(ctx, "git", "-C", policy.Repository.Root, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil || status != "" {
		return errors.New("repository is dirty")
	}
	worktrees, err := command(ctx, "git", "-C", policy.Repository.Root, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(worktrees, "\n") {
		if strings.HasPrefix(line, "worktree ") && beneath(strings.TrimPrefix(line, "worktree "), worktreeRoot) {
			return errors.New("registered worktree collision")
		}
	}
	entries, err := os.ReadDir(worktreeRoot)
	if err != nil || len(entries) != 0 {
		return errors.New("filesystem worktree collision")
	}
	return nil
}

func checkDocker(ctx context.Context, digest string) error {
	if _, err := command(ctx, "docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		return err
	}
	actual, err := command(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", digest)
	if err != nil || actual != digest {
		return errors.New("image mismatch")
	}
	return nil
}

func checkCredentialFile(name string) error {
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("invalid credential file")
	}
	body, err := os.ReadFile(name)
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		return errors.New("empty credential file")
	}
	return nil
}

func migrationSources() []migration.Source {
	return []migration.Source{
		workflow.MigrationSource(), registry.MigrationSource(), gateway.MigrationSource(),
		execution.MigrationSource(), verification.MigrationSource(), approval.MigrationSource(),
		publication.MigrationSource(),
	}
}

func digestMigrations(values []migration.Expected) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = fmt.Fprintf(hash, "%04d\x00%s\x00%s\n", value.Version, value.Name, value.Digest)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func command(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	body, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func beneath(value, root string) bool {
	rel, err := filepath.Rel(root, value)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func cleanAbsolute(value string) bool { return filepath.IsAbs(value) && filepath.Clean(value) == value }
func emit(value report) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}
