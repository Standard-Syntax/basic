package beta

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validPolicy() Policy {
	return Policy{Version: PolicyVersion,
		Repository:    Repository{Owner: "owner", Name: "repo", Root: "/srv/repo", Remote: "origin", RemoteURL: "git@example.invalid:owner/repo.git", BaseBranch: "main", BaseCommit: strings.Repeat("a", 40)},
		Paths:         Paths{Readable: []string{"docs", "go"}, Writable: []string{"go"}, Prohibited: []string{"secrets"}},
		TrustedChecks: []string{"make-check-v1"}, Limits: Limits{MaximumTasks: 1, MaximumChangedFiles: 10, MaximumFileBytes: 1024, MaximumTotalBytes: 4096, ExecutionConcurrency: 1, VerificationConcurrency: 1},
		Images: Images{Execution: "sha256:" + strings.Repeat("b", 64), Verification: "sha256:" + strings.Repeat("c", 64)}}
}

func TestLoadConfigIsStrictAndCanaryPolicyIsClosed(t *testing.T) {
	root := t.TempDir()
	policy := validPolicy()
	policy.Repository = Repository{Owner: CanaryOwner, Name: CanaryRepository,
		Root: filepath.Join(root, "repo"), Remote: "origin",
		RemoteURL:  "git@github.com:Standard-Syntax/basic-beta-canary.git",
		BaseBranch: CanaryBaseBranch, BaseCommit: strings.Repeat("a", 40)}
	policy.Paths = Paths{Readable: []string{"Makefile", "add.go", "add_test.go"},
		Writable: []string{"add.go"}, Prohibited: []string{"Makefile", "add_test.go"}}
	policy.TrustedChecks = []string{"make-check-v1"}
	policy.Limits.MaximumChangedFiles = 1
	value := Config{DatabaseURL: "postgres://example.invalid/database",
		ArtifactRoot: filepath.Join(root, "artifacts"), WorktreeRoot: filepath.Join(root, "worktrees"),
		VerificationWorkspaceRoot:     filepath.Join(root, "verification"),
		ProviderCredentialEnvironment: "ANTHROPIC_API_KEY",
		PublicationCredentialFile:     filepath.Join(root, "github-token"),
		GitPushCredentialFile:         filepath.Join(root, "git-key"), Policy: policy}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, "canary.json")
	if err := os.WriteFile(name, body, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(name)
	if err != nil || loaded.ValidateCanary() != nil {
		t.Fatalf("canary config rejected: load=%v policy=%v", err, loaded.ValidateCanary())
	}
	loaded.Policy.Paths.Readable = append(loaded.Policy.Paths.Readable, "docs")
	if err := loaded.ValidateCanary(); err == nil {
		t.Fatal("widened canary policy accepted")
	}
	body = append(body[:len(body)-1], []byte(`,"github_token":"secret"}`)...)
	if err := os.WriteFile(name, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(name); err == nil {
		t.Fatal("unknown secret-bearing field accepted")
	}
}

func TestPolicyCanonicalDigestIsStable(t *testing.T) {
	value := validPolicy()
	body, digest, err := value.Canonical()
	if err != nil || len(digest) != 64 {
		t.Fatalf("canonical = %s %s %v", body, digest, err)
	}
	decoded, err := DecodePolicy(body)
	if err != nil {
		t.Fatal(err)
	}
	second, secondDigest, _ := decoded.Canonical()
	if !bytes.Equal(body, second) || digest != secondDigest {
		t.Fatal("policy canonicalization changed")
	}
}

func TestPolicyRejectsWideningAndMutableImages(t *testing.T) {
	tests := map[string]func(*Policy){
		"writable outside readable": func(p *Policy) { p.Paths.Writable = []string{"other"} },
		"prohibited writable":       func(p *Policy) { p.Paths.Prohibited = []string{"go"} },
		"mutable image":             func(p *Policy) { p.Images.Execution = "worker:latest" },
		"task expansion":            func(p *Policy) { p.Limits.MaximumTasks = 2 },
		"concurrency expansion":     func(p *Policy) { p.Limits.VerificationConcurrency = 3 },
		"parent traversal": func(p *Policy) {
			p.Paths.Readable = []string{".."}
			p.Paths.Writable = []string{".."}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := validPolicy()
			mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
}

func TestDecodePolicyRejectsUnknownFields(t *testing.T) {
	body, _ := json.Marshal(validPolicy())
	body = append(body[:len(body)-1], []byte(`,"secret":"must-not-appear"}`)...)
	if _, err := DecodePolicy(body); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestWithinAnyHonorsPathBoundaries(t *testing.T) {
	if !WithinAny("go/internal/file.go", []string{"go"}) || WithinAny("gold/file", []string{"go"}) {
		t.Fatal("path boundary mismatch")
	}
}
