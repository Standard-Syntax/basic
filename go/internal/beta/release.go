package beta

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

const ReleaseManifestVersion = "beta_release_manifest.v1"

type ReleaseManifest struct {
	SchemaVersion        string            `json:"schema_version"`
	SourceRepositoryRoot string            `json:"source_repository_root"`
	DeploymentConfigPath string            `json:"deployment_config_path"`
	DeploymentRecordPath string            `json:"deployment_record_path"`
	CanaryConfigPath     string            `json:"canary_config_path"`
	Deployment           DeploymentRecord  `json:"deployment"`
	Toolchains           ReleaseToolchains `json:"toolchains"`
	Canary               CanaryEvidence    `json:"canary"`
	Decision             ReleaseDecision   `json:"decision"`
}

type ReleaseToolchains struct {
	Git    string `json:"git"`
	Go     string `json:"go"`
	UV     string `json:"uv"`
	Docker string `json:"docker"`
}

type CanaryEvidence struct {
	RunID           string               `json:"run_id"`
	TaskID          string               `json:"task_id"`
	PublicationID   string               `json:"publication_id"`
	CandidateCommit string               `json:"candidate_commit"`
	Verification    workflow.ArtifactRef `json:"verification_artifact"`
	Review          workflow.ArtifactRef `json:"review_artifact"`
	Approval        workflow.ArtifactRef `json:"approval_artifact"`
	Publication     workflow.ArtifactRef `json:"publication_artifact"`
	PullRequestURL  string               `json:"pull_request_url"`
}

type ReleaseDecision struct {
	Status      string `json:"status"`
	PrincipalID string `json:"principal_id"`
	DecidedAt   string `json:"decided_at"`
	Reason      string `json:"reason"`
}

func LoadReleaseManifest(path string) (ReleaseManifest, error) {
	if !CleanAbsolute(path) {
		return ReleaseManifest{}, errors.New("release manifest path must be clean and absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return ReleaseManifest{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var value ReleaseManifest
	if err := decoder.Decode(&value); err != nil {
		return ReleaseManifest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ReleaseManifest{}, errors.New("release manifest has trailing content")
	}
	if err := value.Validate(); err != nil {
		return ReleaseManifest{}, err
	}
	return value, nil
}

func (m ReleaseManifest) Validate() error {
	if m.SchemaVersion != ReleaseManifestVersion || !CleanAbsolute(m.SourceRepositoryRoot) ||
		!CleanAbsolute(m.DeploymentConfigPath) || !CleanAbsolute(m.DeploymentRecordPath) ||
		!CleanAbsolute(m.CanaryConfigPath) {
		return errors.New("invalid release manifest identity")
	}
	if err := m.Deployment.Validate(); err != nil {
		return errors.New("invalid release deployment record")
	}
	for _, value := range []string{m.Toolchains.Git, m.Toolchains.Go, m.Toolchains.UV, m.Toolchains.Docker} {
		if strings.TrimSpace(value) == "" {
			return errors.New("complete release toolchain versions are required")
		}
	}
	if err := m.Canary.Validate(); err != nil {
		return err
	}
	return m.Decision.Validate()
}

func (m ReleaseManifest) Digest() (string, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func (e CanaryEvidence) Validate() error {
	for _, value := range []string{e.RunID, e.TaskID, e.PublicationID} {
		if _, err := uuid.Parse(value); err != nil {
			return errors.New("invalid canary evidence identity")
		}
	}
	if !commitPattern.MatchString(e.CandidateCommit) || strings.TrimSpace(e.PullRequestURL) == "" {
		return errors.New("invalid canary commit or pull request")
	}
	for _, ref := range []workflow.ArtifactRef{e.Verification, e.Review, e.Approval, e.Publication} {
		if err := ref.Validate(); err != nil || ref.URI != "artifact://sha256/"+ref.Digest {
			return errors.New("invalid canary artifact reference")
		}
	}
	return nil
}

func (d ReleaseDecision) Validate() error {
	if d.Status != "go" && d.Status != "no_go" {
		return errors.New("release decision must be go or no_go")
	}
	if _, err := uuid.Parse(d.PrincipalID); err != nil {
		return errors.New("invalid release decision principal")
	}
	when, err := time.Parse(time.RFC3339Nano, d.DecidedAt)
	_, offset := when.Zone()
	if err != nil || offset != 0 || strings.TrimSpace(d.Reason) == "" {
		return errors.New("invalid release decision record")
	}
	return nil
}
