package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/controlapi"
	"github.com/google/uuid"
)

func TestOptionalPublicationIsNilInterface(t *testing.T) {
	service, err := buildPublication(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if service != nil {
		t.Fatalf("omitted publication returned non-nil interface: %#v", service)
	}
}

func TestHTTPWriteTimeoutExceedsDetachedOperationBudget(t *testing.T) {
	server := newHTTPServer("127.0.0.1:0", nil)
	if server.WriteTimeout != 3*time.Minute ||
		server.WriteTimeout <= controlapi.MaximumDetachedOperationTimeout {
		t.Fatalf("write timeout %s must exceed detached operation budget %s",
			server.WriteTimeout, controlapi.MaximumDetachedOperationTimeout)
	}
}

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
			token := sha256.Sum256([]byte("test-token"))
			value := config{Listen: "127.0.0.1:0", DatabaseURL: "postgres://example",
				ArtifactRoot: filepath.Join(root, "artifacts"), RepositoryRoot: repository}
			value.ServiceActorID = uuid.NewString()
			value.Principals = []controlapi.Principal{{
				ID: uuid.NewString(), TokenSHA256: hex.EncodeToString(token[:]),
				Roles: []controlapi.Role{controlapi.RoleOperator},
			}}
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
