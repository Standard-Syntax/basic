package verification

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
)

func TestResultJSONKeysRemainPascalCase(t *testing.T) {
	value := Result{
		VerificationID: "verification", CandidateCommit: "candidate",
		ReportArtifact: workflow.ArtifactRef{URI: "artifact", Digest: "digest"},
		Coverage:       []CriterionCoverage{}, Passed: true, Replay: true,
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	want := []string{"VerificationID", "CandidateCommit", "ReportArtifact", "Coverage", "Passed", "Replay"}
	got := make([]string, 0, len(decoded))
	for _, key := range want {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("missing persisted key %q in %s", key, body)
		}
		got = append(got, key)
	}
	if !reflect.DeepEqual(got, want) || len(decoded) != len(want) {
		t.Fatalf("persisted keys = %#v", decoded)
	}
}
