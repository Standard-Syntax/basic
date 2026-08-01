package beta

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
)

const DeploymentVersion = "beta_deployment.v1"

var immutableImagePattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Deployment is the strict, secret-free operator envelope for one exact beta
// image set. Sensitive values remain in separately mounted owner-only files.
type Deployment struct {
	SchemaVersion     string                `json:"schema_version"`
	SourceCommit      string                `json:"source_commit"`
	Policy            Policy                `json:"beta_policy"`
	Services          DeploymentServices    `json:"services"`
	Images            DeploymentImages      `json:"images"`
	Mounts            DeploymentMounts      `json:"mounts"`
	Credentials       DeploymentCredentials `json:"credentials"`
	DockerSocketGroup int                   `json:"docker_socket_group"`
}

type DeploymentServices struct {
	APIConfig      string `json:"api_config"`
	WorkflowConfig string `json:"workflow_config"`
	APIListen      string `json:"api_listen"`
	WorkflowListen string `json:"workflow_listen"`
}

type DeploymentImages struct {
	API          string `json:"api_service"`
	Workflow     string `json:"workflow_service"`
	Execution    string `json:"execution_worker"`
	Verification string `json:"verification_worker"`
}

type DeploymentMounts struct {
	Repository    string `json:"repository"`
	CAS           string `json:"cas"`
	Worktrees     string `json:"worktrees"`
	Verification  string `json:"verification"`
	Manifests     string `json:"manifests"`
	Prompts       string `json:"prompts"`
	Configuration string `json:"configuration"`
}

type DeploymentCredentials struct {
	MiniMax          string `json:"minimax"`
	DatabasePassword string `json:"database_password"`
	GitHub           string `json:"github,omitempty"`
	GitPush          string `json:"git_push,omitempty"`
}

func LoadDeployment(path string) (Deployment, error) {
	if !CleanAbsolute(path) {
		return Deployment{}, errors.New("deployment path must be clean and absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return Deployment{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var value Deployment
	if err := decoder.Decode(&value); err != nil {
		return Deployment{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Deployment{}, errors.New("deployment has trailing content")
	}
	if err := value.Validate(); err != nil {
		return Deployment{}, err
	}
	return value, nil
}

func (d *Deployment) Validate() error { // skipcq: GO-R1005 -- strict fail-closed envelope validation
	if d.SchemaVersion != DeploymentVersion || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(d.SourceCommit) ||
		d.DockerSocketGroup <= 0 || d.Services.APIListen == "" || d.Services.WorkflowListen == "" {
		return errors.New("invalid beta deployment identity")
	}
	if err := d.Policy.Validate(); err != nil {
		return errors.New("invalid beta deployment policy")
	}
	images := []string{d.Images.API, d.Images.Workflow, d.Images.Execution, d.Images.Verification}
	for _, image := range images {
		if !immutableImagePattern.MatchString(image) {
			return errors.New("deployment images must be immutable IDs")
		}
	}
	if d.Policy.Images.Execution != d.Images.Execution || d.Policy.Images.Verification != d.Images.Verification {
		return errors.New("deployment worker images do not match beta policy")
	}
	paths := []string{d.Services.APIConfig, d.Services.WorkflowConfig, d.Mounts.Repository,
		d.Mounts.CAS, d.Mounts.Worktrees, d.Mounts.Verification, d.Mounts.Manifests,
		d.Mounts.Prompts, d.Mounts.Configuration, d.Credentials.MiniMax, d.Credentials.DatabasePassword}
	if d.Credentials.GitHub != "" {
		paths = append(paths, d.Credentials.GitHub)
	}
	if d.Credentials.GitPush != "" {
		paths = append(paths, d.Credentials.GitPush)
	}
	for _, path := range paths {
		if !CleanAbsolute(path) {
			return errors.New("deployment paths must be clean and absolute")
		}
	}
	if d.Policy.Repository.Root != d.Mounts.Repository {
		return errors.New("deployment repository does not match beta policy")
	}
	return nil
}

func (d *Deployment) Digest() (string, error) {
	body, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
