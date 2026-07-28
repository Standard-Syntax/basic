package contracts

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"google.golang.org/protobuf/proto"
)

func contractFixture(t *testing.T, name string) []byte {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	path := filepath.Join(
		filepath.Dir(source), "..", "..", "..", "..", "tests", "contracts", "v1", "specification", name,
	)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPythonSpecificationFixturesMapInGo(t *testing.T) {
	var request reasoningv1.SpecificationRequest
	if err := proto.Unmarshal(contractFixture(t, "request.bin"), &request); err != nil {
		t.Fatal(err)
	}
	mappedRequest, err := MapSpecificationRequest(&request)
	if err != nil {
		t.Fatal(err)
	}
	if mappedRequest.Envelope.RequestID != "request-spec-001" {
		t.Fatalf("request ID = %q", mappedRequest.Envelope.RequestID)
	}
	var proposal reasoningv1.SpecificationProposal
	if err := proto.Unmarshal(contractFixture(t, "proposal.bin"), &proposal); err != nil {
		t.Fatal(err)
	}
	mappedProposal, err := MapSpecificationProposal(&proposal)
	if err != nil {
		t.Fatal(err)
	}
	if got := mappedProposal.AcceptanceCriteria[0].ID; got != "AC-001" {
		t.Fatalf("criterion ID = %q", got)
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(&proposal)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(contractFixture(t, "proposal.bin")) {
		t.Fatal("Go deterministic serialization differs from Python fixture")
	}
}

func TestSpecificationEnvelopeFailsClosedOnAuthorityElevation(t *testing.T) {
	var request reasoningv1.SpecificationRequest
	if err := proto.Unmarshal(contractFixture(t, "request.bin"), &request); err != nil {
		t.Fatal(err)
	}
	for name, elevate := range map[string]func(*reasoningv1.AuthorityConstraints){
		"mutate state": func(value *reasoningv1.AuthorityConstraints) {
			value.MayMutateKernelState = true
		},
		"execute commands": func(value *reasoningv1.AuthorityConstraints) {
			value.MayExecuteCommands = true
		},
		"modify files": func(value *reasoningv1.AuthorityConstraints) {
			value.MayModifyFiles = true
		},
		"expand scope": func(value *reasoningv1.AuthorityConstraints) {
			value.MayExpandScope = true
		},
		"approve": func(value *reasoningv1.AuthorityConstraints) {
			value.MayApproveWork = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			cloned := proto.Clone(&request).(*reasoningv1.SpecificationRequest)
			elevate(cloned.Envelope.Authority)
			if _, err := MapSpecificationRequest(cloned); err == nil {
				t.Fatal("elevated authority accepted")
			}
		})
	}
}

func TestMalformedSpecificationEnvelopeIsRejected(t *testing.T) {
	var request reasoningv1.SpecificationRequest
	if err := proto.Unmarshal(contractFixture(t, "request.bin"), &request); err != nil {
		t.Fatal(err)
	}
	request.Envelope.ExpiresAt = request.Envelope.CreatedAt
	if _, err := MapSpecificationRequest(&request); err == nil {
		t.Fatal("invalid expiry accepted")
	}
}
