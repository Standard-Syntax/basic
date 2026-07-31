package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Standard-Syntax/basic/go/internal/beta"
)

func TestLoadConfigRequiresCleanAbsoluteRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "api.json")
	for _, test := range []struct {
		name       string
		repository string
		wantError  bool
	}{
		{name: "missing", repository: `""`, wantError: true},
		{name: "relative", repository: `"repository"`, wantError: true},
		{name: "unclean", repository: `"` + root + `/nested/.."`, wantError: true},
		{name: "absolute", repository: `"` + root + `"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := strings.Trim(test.repository, `"`)
			value := config{Listen: "127.0.0.1:0", DatabaseURL: "postgres://example",
				ArtifactRoot: filepath.Join(root, "artifacts"), RepositoryRoot: repository}
			value.Policy = beta.Policy{Version: beta.PolicyVersion,
				Repository: beta.Repository{Owner: "owner", Name: "repo", Root: repository, Remote: "origin", RemoteURL: "https://example.invalid/repo.git", BaseBranch: "main", BaseCommit: strings.Repeat("a", 40)},
				Paths:      beta.Paths{Readable: []string{"docs"}, Writable: []string{"docs"}, Prohibited: []string{"secret"}}, TrustedChecks: []string{"check"},
				Limits: beta.Limits{MaximumTasks: 1, MaximumChangedFiles: 1, MaximumFileBytes: 1, MaximumTotalBytes: 1, ExecutionConcurrency: 1, VerificationConcurrency: 1},
				Images: beta.Images{Execution: "sha256:" + strings.Repeat("a", 64), Verification: "sha256:" + strings.Repeat("b", 64)}}
			body, marshalErr := json.Marshal(value)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadConfig(configPath)
			if (err != nil) != test.wantError {
				t.Fatalf("loadConfig error=%v wantError=%v", err, test.wantError)
			}
		})
	}
}
