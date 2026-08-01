package beta

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
)

// DeploymentRecord is the immutable, secret-free packaging bill of materials.
type DeploymentRecord struct {
	SchemaVersion       string            `json:"schema_version"`
	SourceCommit        string            `json:"source_commit"`
	MigrationDigest     string            `json:"migration_digest"`
	ManifestDigests     map[string]string `json:"manifest_digests"`
	PromptDigests       map[string]string `json:"prompt_digests"`
	Images              DeploymentImages  `json:"images"`
	GitVersion          string            `json:"git_version"`
	GoVersion           string            `json:"go_version"`
	ToolchainVersion    string            `json:"toolchain_version"`
	ConfigurationDigest string            `json:"configuration_digest"`
}

const DeploymentRecordVersion = "beta_deployment_record.v1"

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func LoadDeploymentRecord(path string) (DeploymentRecord, error) {
	if !CleanAbsolute(path) {
		return DeploymentRecord{}, errors.New("deployment record path must be clean and absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return DeploymentRecord{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var value DeploymentRecord
	if err := decoder.Decode(&value); err != nil {
		return DeploymentRecord{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return DeploymentRecord{}, errors.New("deployment record has trailing content")
	}
	if err := value.Validate(); err != nil {
		return DeploymentRecord{}, err
	}
	return value, nil
}

func (r DeploymentRecord) Validate() error {
	if r.SchemaVersion != DeploymentRecordVersion || !commitPattern.MatchString(r.SourceCommit) ||
		!digestPattern.MatchString(r.MigrationDigest) ||
		!digestPattern.MatchString(r.ConfigurationDigest) ||
		strings.TrimSpace(r.GitVersion) == "" || strings.TrimSpace(r.GoVersion) == "" ||
		strings.TrimSpace(r.ToolchainVersion) == "" {
		return errors.New("invalid deployment record identity")
	}
	for _, image := range []string{r.Images.API, r.Images.Workflow, r.Images.Execution, r.Images.Verification} {
		if !immutableImagePattern.MatchString(image) {
			return errors.New("deployment record images must be immutable IDs")
		}
	}
	if err := validateDigestMap("manifest", r.ManifestDigests); err != nil {
		return err
	}
	return validateDigestMap("prompt", r.PromptDigests)
}

func validateDigestMap(label string, values map[string]string) error {
	if len(values) == 0 {
		return errors.New(label + " digests are required")
	}
	for name, digest := range values {
		if strings.TrimSpace(name) == "" || !digestPattern.MatchString(digest) {
			return errors.New("invalid " + label + " digest")
		}
	}
	return nil
}
