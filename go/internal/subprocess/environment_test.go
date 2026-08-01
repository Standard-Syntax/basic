package subprocess

import (
	"slices"
	"strings"
	"testing"
)

func TestClosedEnvironmentsExcludeHostileParentValues(t *testing.T) {
	t.Setenv("GIT_DIR", "/hostile/git")
	t.Setenv("GIT_WORK_TREE", "/hostile/worktree")
	t.Setenv("ANTHROPIC_API_KEY", "hostile-provider-credential")
	environment := Git("GIT_INDEX_FILE=/private/index")
	for _, entry := range environment {
		if strings.HasPrefix(entry, "GIT_DIR=") || strings.HasPrefix(entry, "GIT_WORK_TREE=") ||
			strings.HasPrefix(entry, "ANTHROPIC_API_KEY=") {
			t.Fatalf("host environment escaped: %q", entry)
		}
	}
	if !slices.Contains(environment, "GIT_INDEX_FILE=/private/index") {
		t.Fatalf("operation-specific variable missing: %v", environment)
	}
	if _, err := RemoteGit("/safe/key;touch-pwned"); err == nil {
		t.Fatal("unsafe SSH key path accepted at command sink")
	}
}
