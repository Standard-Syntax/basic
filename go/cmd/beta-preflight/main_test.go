package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Standard-Syntax/basic/go/internal/beta"
)

func preflightTestConfig(root string) config {
	return config{DatabaseURL: "postgres://example.invalid/database",
		ArtifactRoot: filepath.Join(root, "artifacts"), WorktreeRoot: filepath.Join(root, "worktrees"),
		VerificationWorkspaceRoot: filepath.Join(root, "verification"), ProviderCredentialEnvironment: "TEST_PROVIDER_KEY",
		PublicationCredentialFile: filepath.Join(root, "token"),
		Policy: beta.Policy{Version: beta.PolicyVersion,
			Repository: beta.Repository{Owner: "owner", Name: "repo", Root: filepath.Join(root, "repo"), Remote: "origin", RemoteURL: "https://example.invalid/owner/repo.git", BaseBranch: "main", BaseCommit: strings.Repeat("a", 40)},
			Paths:      beta.Paths{Readable: []string{"docs"}, Writable: []string{"docs"}, Prohibited: []string{"secrets"}}, TrustedChecks: []string{"make-check-v1"},
			Limits: beta.Limits{MaximumTasks: 1, MaximumChangedFiles: 1, MaximumFileBytes: 1024, MaximumTotalBytes: 1024, ExecutionConcurrency: 1, VerificationConcurrency: 1},
			Images: beta.Images{Execution: "sha256:" + strings.Repeat("b", 64), Verification: "sha256:" + strings.Repeat("c", 64)}}}
}

func TestLoadConfigIsStrictAndRejectsUnknownFields(t *testing.T) {
	value := preflightTestConfig(t.TempDir())
	body, _ := json.Marshal(value)
	body = append(body[:len(body)-1], []byte(`,"credential":"do-not-print"}`)...)
	name := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(name, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(name); err == nil {
		t.Fatal("unknown configuration field accepted")
	}
}

func TestCredentialFileRequiresExact0600AndContent(t *testing.T) {
	name := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(name, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkCredentialFile(name); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := checkCredentialFile(name); err == nil {
		t.Fatal("broad credential mode accepted")
	}
}

func TestReportContainsNoCredentialMaterial(t *testing.T) {
	body, err := json.Marshal(report{Status: "not_ready", FailureCodes: []string{"PROVIDER_CREDENTIAL_UNAVAILABLE"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"ANTHROPIC_API_KEY", "token", "postgres://"} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("report disclosed %q", secret)
		}
	}
}
