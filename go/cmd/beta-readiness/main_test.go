package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestMainExitRejectsInvalidManifestWithoutDependencies(t *testing.T) {
	oldArgs, oldFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args, flag.CommandLine = oldArgs, oldFlags })
	flag.CommandLine = flag.NewFlagSet("beta-readiness", flag.ContinueOnError)
	path := filepath.Join(t.TempDir(), "release.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":"wrong"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"beta-readiness", "-manifest", path}
	if code := mainExit(); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}
