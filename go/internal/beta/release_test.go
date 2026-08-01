package beta

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

func validDeploymentRecord() DeploymentRecord {
	digest := strings.Repeat("a", 64)
	return DeploymentRecord{
		SchemaVersion: DeploymentRecordVersion, SourceCommit: strings.Repeat("b", 40),
		MigrationDigest: digest, ManifestDigests: map[string]string{"implementation.json": digest},
		PromptDigests: map[string]string{"implementation.md": digest}, Images: validDeployment().Images,
		GitVersion: "git version 2", GoVersion: "go1.26", ToolchainVersion: "go1.26",
		ConfigurationDigest: digest,
	}
}

func validReleaseManifest() ReleaseManifest {
	digest := strings.Repeat("c", 64)
	return ReleaseManifest{
		SchemaVersion: ReleaseManifestVersion, SourceRepositoryRoot: "/srv/basic/source",
		DeploymentConfigPath: "/srv/basic/release/deployment.json",
		DeploymentRecordPath: "/srv/basic/release/deployment-record.json",
		CanaryConfigPath:     "/srv/basic/release/canary.json", Deployment: validDeploymentRecord(),
		Toolchains: ReleaseToolchains{Git: "git version 2", Go: "go1.26", UV: "uv 0.11", Docker: "Docker 29"},
		Canary: CanaryEvidence{RunID: uuid.NewString(), TaskID: uuid.NewString(), PublicationID: uuid.NewString(),
			CandidateCommit: strings.Repeat("d", 40),
			Verification:    workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest},
			Review:          workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest},
			Approval:        workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest},
			Publication:     workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest},
			PullRequestURL:  "https://github.com/Standard-Syntax/basic-beta-canary/pull/1"},
		Decision: ReleaseDecision{Status: "go", PrincipalID: uuid.NewString(),
			DecidedAt: "2026-07-31T18:00:00Z", Reason: "all evidence inspected"},
	}
}

func TestReleaseManifestStrictLoadAndDigest(t *testing.T) {
	value := validReleaseManifest()
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	first, err := value.Digest()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "release.json")
	body, _ := json.Marshal(value)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadReleaseManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := loaded.Digest()
	if first != second {
		t.Fatalf("release digest changed: %s != %s", first, second)
	}
	if err := os.WriteFile(path, append(body, []byte(` {}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReleaseManifest(path); err == nil {
		t.Fatal("trailing release content was accepted")
	}
}

func TestReleaseManifestRejectsIncompleteEvidenceAndDecision(t *testing.T) {
	for name, mutate := range map[string]func(*ReleaseManifest){
		"mutable image":       func(value *ReleaseManifest) { value.Deployment.Images.API = "api:latest" },
		"missing toolchain":   func(value *ReleaseManifest) { value.Toolchains.UV = "" },
		"invalid artifact":    func(value *ReleaseManifest) { value.Canary.Review.URI = "file:///tmp/review" },
		"invalid decision":    func(value *ReleaseManifest) { value.Decision.Status = "pending" },
		"local decision time": func(value *ReleaseManifest) { value.Decision.DecidedAt = "2026-07-31T13:00:00-05:00" },
	} {
		t.Run(name, func(t *testing.T) {
			value := validReleaseManifest()
			mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatal("invalid release manifest was accepted")
			}
		})
	}
}

func TestDeploymentRecordStrictLoad(t *testing.T) {
	value := validDeploymentRecord()
	path := filepath.Join(t.TempDir(), "deployment-record.json")
	body, _ := json.Marshal(value)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadDeploymentRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SourceCommit != value.SourceCommit {
		t.Fatal("deployment record identity changed")
	}
	body = append(body[:len(body)-1], []byte(`,"unknown":true}`)...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDeploymentRecord(path); err == nil {
		t.Fatal("unknown deployment record field was accepted")
	}
}
