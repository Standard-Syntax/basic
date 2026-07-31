package beta

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

const (
	CanaryOwner      = "Standard-Syntax"
	CanaryRepository = "basic-beta-canary"
	CanaryBaseBranch = "main"
)

var canaryCredentialPathPattern = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)

// Config is the shared strict configuration envelope used by production
// preflight and the separately credentialed GitHub canary commands. Secret
// values are never members of the envelope; only mounted credential paths are.
type Config struct {
	DatabaseURL                   string `json:"database_url"`
	ArtifactRoot                  string `json:"artifact_root"`
	WorktreeRoot                  string `json:"worktree_root"`
	VerificationWorkspaceRoot     string `json:"verification_workspace_root"`
	ProviderCredentialEnvironment string `json:"provider_credential_environment"`
	PublicationCredentialFile     string `json:"publication_credential_file"`
	GitPushCredentialFile         string `json:"git_push_credential_file,omitempty"`
	Policy                        Policy `json:"beta_policy"`
}

func LoadConfig(name string) (Config, error) {
	if !CleanAbsolute(name) {
		return Config{}, errors.New("config path must be clean and absolute")
	}
	file, err := os.Open(name)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var value Config
	if err := decoder.Decode(&value); err != nil {
		return Config{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("trailing configuration")
	}
	if value.DatabaseURL == "" || value.ProviderCredentialEnvironment == "" ||
		!CleanAbsolute(value.ArtifactRoot) || !CleanAbsolute(value.WorktreeRoot) ||
		!CleanAbsolute(value.VerificationWorkspaceRoot) ||
		!CleanAbsolute(value.PublicationCredentialFile) ||
		(value.GitPushCredentialFile != "" && !CleanAbsolute(value.GitPushCredentialFile)) {
		return Config{}, errors.New("incomplete preflight configuration")
	}
	if err := value.Policy.Validate(); err != nil {
		return Config{}, err
	}
	return value, nil
}

// ValidateCanary narrows the general beta policy to the one operator-owned
// canary repository and fixture. It intentionally does not accept aliases or
// additional readable/writable paths.
func (c Config) ValidateCanary() error {
	if c.Policy.Repository.Owner != CanaryOwner ||
		c.Policy.Repository.Name != CanaryRepository ||
		c.Policy.Repository.Remote != "origin" ||
		c.Policy.Repository.RemoteURL != "git@github.com:Standard-Syntax/basic-beta-canary.git" ||
		c.Policy.Repository.BaseBranch != CanaryBaseBranch ||
		c.ProviderCredentialEnvironment != "ANTHROPIC_API_KEY" ||
		c.GitPushCredentialFile == "" ||
		!canaryCredentialPathPattern.MatchString(c.GitPushCredentialFile) ||
		!slices.Equal(c.Policy.Paths.Readable, []string{"Makefile", "add.go", "add_test.go"}) ||
		!slices.Equal(c.Policy.Paths.Writable, []string{"add.go"}) ||
		!slices.Equal(c.Policy.Paths.Prohibited, []string{"Makefile", "add_test.go"}) ||
		!slices.Equal(c.Policy.TrustedChecks, []string{"make-check-v1"}) ||
		c.Policy.Limits.MaximumChangedFiles != 1 {
		return errors.New("configuration does not match the dedicated canary policy")
	}
	return nil
}

func CleanAbsolute(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value
}
