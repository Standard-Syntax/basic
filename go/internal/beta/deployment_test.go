package beta

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validDeployment() Deployment {
	image := func(value string) string { return "sha256:" + strings.Repeat(value, 64) }
	minimaxCredentialPath := "/run/credentials/minimax"   // skipcq: SCT-A000 -- fixture path, not a credential
	databaseCredentialPath := "/run/credentials/postgres" // skipcq: SCT-A000 -- fixture path, not a credential
	policy := validPolicy()
	policy.Images = Images{Execution: image("c"), Verification: image("d")}
	return Deployment{SchemaVersion: DeploymentVersion, SourceCommit: strings.Repeat("a", 40),
		Policy: policy, DockerSocketGroup: 999,
		Services:    DeploymentServices{APIConfig: "/etc/basic/api.json", WorkflowConfig: "/etc/basic/workflow.json", APIListen: "127.0.0.1:8080", WorkflowListen: "workflow-service:8081"},
		Images:      DeploymentImages{API: image("a"), Workflow: image("b"), Execution: image("c"), Verification: image("d")},
		Mounts:      DeploymentMounts{Repository: policy.Repository.Root, CAS: "/var/lib/basic/cas", Worktrees: "/var/lib/basic/worktrees", Verification: "/var/lib/basic/verification", Manifests: "/etc/basic/manifests", Prompts: "/etc/basic/prompts", Configuration: "/etc/basic"},
		Credentials: DeploymentCredentials{MiniMax: minimaxCredentialPath, DatabasePassword: databaseCredentialPath}}
}

func TestDeploymentStrictlyRejectsUnknownFieldsAndMutableImages(t *testing.T) {
	value := validDeployment()
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	value.Images.API = "basic-api:latest"
	if err := value.Validate(); err == nil {
		t.Fatal("mutable image accepted")
	}
	body, _ := json.Marshal(validDeployment())
	body = append(body[:len(body)-1], []byte(`,"unknown":true}`)...)
	path := filepath.Join(t.TempDir(), "beta.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDeployment(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field = %v", err)
	}
}
