package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigRequiresCleanAbsoluteRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "api.json")
	base := `{"listen":"127.0.0.1:0","database_url":"postgres://example",` +
		`"artifact_root":"` + filepath.Join(root, "artifacts") + `",` +
		`"repository_root":%s}`
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
			body := strings.Replace(base, "%s", test.repository, 1)
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
