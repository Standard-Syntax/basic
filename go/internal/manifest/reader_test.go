package manifest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	return filepath.Join(filepath.Dir(source), "..", "..", "..", "tests", "contracts", "v1", "manifest", name)
}

func TestGoldenManifestAndDigest(t *testing.T) {
	data, err := os.ReadFile(fixturePath(t, "implementation.json"))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile(fixturePath(t, "implementation.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, canonical, digest, err := Read(data)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Stage != "implementation" {
		t.Fatalf("stage = %q", manifest.Stage)
	}
	if string(canonical) != strings.TrimSpace(string(data)) {
		t.Fatal("fixture is not canonical RFC 8785 JSON")
	}
	if digest != strings.TrimSpace(string(expected)) {
		t.Fatalf("digest = %s; want %s", digest, expected)
	}
}

func TestReadRejectsUnknownUnsafeAndInvalidValues(t *testing.T) {
	tests := map[string]string{
		"unknown authority":   `{"schema_version":"1","authority":true}`,
		"invalid JSON number": `{"model":{"temperature":NaN}}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := Read([]byte(input)); err == nil {
				t.Fatal("Read unexpectedly succeeded")
			}
		})
	}
}

func TestValidateRejectsUnsafeAndInvalidURI(t *testing.T) {
	data, err := os.ReadFile(fixturePath(t, "implementation.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, _, err := Read(data)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Tools.ArbitraryNetwork = true
	if err := manifest.Validate(); err == nil {
		t.Fatal("unsafe network permission accepted")
	}
	manifest.Tools.ArbitraryNetwork = false
	manifest.Prompt.ArtifactURI = "https://example.invalid/prompt"
	if err := manifest.Validate(); err == nil {
		t.Fatal("unrestricted prompt URL accepted")
	}
}
