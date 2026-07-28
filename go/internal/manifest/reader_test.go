package manifest

import (
	"encoding/json"
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

func TestReadRejectsMissingRequiredFields(t *testing.T) {
	data, err := os.ReadFile(fixturePath(t, "implementation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	delete(value, "metadata")
	missingTopLevel, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Read(missingTopLevel); err == nil {
		t.Fatal("missing top-level field accepted")
	}

	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	delete(value["tools"].(map[string]any), "direct_file_write")
	missingNested, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Read(missingNested); err == nil {
		t.Fatal("missing nested field accepted")
	}
}

func TestGoldenManifestAndDigest(t *testing.T) {
	for _, stage := range validStages {
		t.Run(stage, func(t *testing.T) {
			data, err := os.ReadFile(fixturePath(t, stage+".json"))
			if err != nil {
				t.Fatal(err)
			}
			expected, err := os.ReadFile(fixturePath(t, stage+".sha256"))
			if err != nil {
				t.Fatal(err)
			}
			manifest, canonical, digest, err := Read(data)
			if err != nil {
				t.Fatal(err)
			}
			if manifest.Stage != stage {
				t.Fatalf("stage = %q", manifest.Stage)
			}
			if string(canonical) != strings.TrimSpace(string(data)) {
				t.Fatal("fixture is not canonical RFC 8785 JSON")
			}
			if digest != strings.TrimSpace(string(expected)) {
				t.Fatalf("digest = %s; want %s", digest, expected)
			}
		})
	}
}

func TestReadRejectsUnknownUnsafeAndInvalidValues(t *testing.T) {
	data, err := os.ReadFile(fixturePath(t, "implementation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]any
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(map[string]any){
		"unknown authority": func(value map[string]any) {
			value["authority"] = true
		},
		"unsafe permission": func(value map[string]any) {
			value["tools"].(map[string]any)["arbitrary_shell"] = true
		},
		"invalid agent name": func(value map[string]any) {
			value["agent"].(map[string]any)["name"] = "Invalid Name"
		},
		"invalid agent version": func(value map[string]any) {
			value["agent"].(map[string]any)["version"] = "v1"
		},
		"invalid metadata": func(value map[string]any) {
			value["metadata"].(map[string]any)["labels"] = []any{"Invalid Label"}
		},
		"stage output mismatch": func(value map[string]any) {
			value["output"].(map[string]any)["schema"] = "review_proposal.v1"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			input := cloneJSONMap(t, fixture)
			mutate(input)
			encoded, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := Read(encoded); err == nil {
				t.Fatal("Read unexpectedly succeeded")
			}
		})
	}
	if _, _, _, err := Read([]byte(`{"model":{"temperature":NaN}}`)); err == nil {
		t.Fatal("invalid JSON number accepted")
	}
}

func cloneJSONMap(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
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

func TestValidateRejectsStageOutputMismatch(t *testing.T) {
	data, err := os.ReadFile(fixturePath(t, "implementation.json"))
	if err != nil {
		t.Fatal(err)
	}
	value, _, _, err := Read(data)
	if err != nil {
		t.Fatal(err)
	}
	value.Output.Schema = "review_proposal.v1"
	if err := value.Validate(); err == nil {
		t.Fatal("stage/output mismatch accepted")
	}
}

func TestManifestContentChangeCausesDigestDrift(t *testing.T) {
	data, err := os.ReadFile(fixturePath(t, "implementation.json"))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile(fixturePath(t, "implementation.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	value["metadata"].(map[string]any)["description"] = "Changed description"
	changed, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, digest, err := Read(changed); err != nil {
		t.Fatal(err)
	} else if digest == strings.TrimSpace(string(expected)) {
		t.Fatal("changed manifest retained fixture digest")
	}
}
