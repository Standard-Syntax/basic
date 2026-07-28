package contracts

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"google.golang.org/protobuf/proto"
)

func implementationFixture(t *testing.T, name string) []byte {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	path := filepath.Join(
		filepath.Dir(source), "..", "..", "..", "..", "tests", "contracts", "v1",
		"implementation", name,
	)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func validImplementation(
	t *testing.T,
) (ImplementationRequest, *reasoningv1.ImplementationProposal) {
	t.Helper()
	var requestPB reasoningv1.ImplementationRequest
	if err := proto.Unmarshal(implementationFixture(t, "request.bin"), &requestPB); err != nil {
		t.Fatal(err)
	}
	request, err := MapImplementationRequest(&requestPB)
	if err != nil {
		t.Fatal(err)
	}
	var proposal reasoningv1.ImplementationProposal
	if err := proto.Unmarshal(implementationFixture(t, "proposal.bin"), &proposal); err != nil {
		t.Fatal(err)
	}
	return request, &proposal
}

func TestImplementationOperationsMapAndRoundTrip(t *testing.T) {
	request, proposal := validImplementation(t)
	if len(request.RepositoryContext) != 1 {
		t.Fatal("repository context was discarded")
	}
	mapped, err := MapImplementationProposal(proposal, request)
	if err != nil {
		t.Fatal(err)
	}
	want := []FileOperation{FileCreate, FileUpdate, FileDelete}
	for index, operation := range want {
		if mapped.Changes[index].Operation != operation {
			t.Fatalf("operation %d = %s", index, mapped.Changes[index].Operation)
		}
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, implementationFixture(t, "proposal.bin")) {
		t.Fatal("Go deterministic serialization differs from Python fixture")
	}
}

func TestScopeChangeRequestIsPreservedButNotApplied(t *testing.T) {
	request, proposal := validImplementation(t)
	proposal.ScopeChangeRequest = &reasoningv1.ScopeChangeRequest{
		Summary:                         "Need a wider read scope.",
		RequestedReadablePaths:          []string{"docs"},
		RequestedAcceptanceCriterionIds: []string{"AC-004"},
		RequestedCheckIds:               []string{"CHECK-DOCS"},
	}
	mapped, err := MapImplementationProposal(proposal, request)
	if err != nil {
		t.Fatal(err)
	}
	if mapped.ScopeChangeRequest == nil || mapped.ScopeChangeRequest.Summary == "" {
		t.Fatal("scope change request was discarded")
	}
	if len(request.ReadablePaths) != 1 || request.ReadablePaths[0] != "go" {
		t.Fatal("scope change request mutated kernel-selected scope")
	}
}

func TestInvalidImplementationProposalsFailWithoutPartialResult(t *testing.T) {
	tests := map[string]func(*reasoningv1.ImplementationProposal){
		"absolute path": func(value *reasoningv1.ImplementationProposal) {
			value.Changes[0].Path = "/etc/passwd"
		},
		"path traversal": func(value *reasoningv1.ImplementationProposal) {
			value.Changes[0].Path = "../escape"
		},
		"malformed digest": func(value *reasoningv1.ImplementationProposal) {
			value.Changes[1].ExpectedOriginalSha256 = "bad"
		},
		"request mismatch": func(value *reasoningv1.ImplementationProposal) {
			value.Identity.RequestId = "stale"
		},
		"task mismatch": func(value *reasoningv1.ImplementationProposal) {
			value.Identity.TaskId = proto.String("TASK-999")
		},
		"incomplete coverage": func(value *reasoningv1.ImplementationProposal) {
			value.Changes = value.Changes[:2]
		},
		"undeclared check": func(value *reasoningv1.ImplementationProposal) {
			value.RequestedDeclaredCheckIds = []string{"CHECK-DEPLOY"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request, proposal := validImplementation(t)
			mutate(proposal)
			mapped, err := MapImplementationProposal(proposal, request)
			if err == nil {
				t.Fatal("invalid proposal accepted")
			}
			if len(mapped.Changes) != 0 {
				t.Fatal("partial result returned")
			}
		})
	}
}

func TestAllStableRejectionCodesRoundTripInGo(t *testing.T) {
	codes := []reasoningv1.RejectionCode{
		reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
		reasoningv1.RejectionCode_REJECTION_CODE_REQUEST_MISMATCH,
		reasoningv1.RejectionCode_REJECTION_CODE_AUTHORITY_VIOLATION,
		reasoningv1.RejectionCode_REJECTION_CODE_SCOPE_VIOLATION,
		reasoningv1.RejectionCode_REJECTION_CODE_REQUIRED_COVERAGE_MISSING,
	}
	for _, code := range codes {
		value := &reasoningv1.ProposalRejection{
			Code: code, Summary: "deterministic rejection", RequestId: "request-impl-001",
			RunId: "run-001", TaskId: proto.String("TASK-001"), Attempt: 1,
		}
		data, err := proto.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded reasoningv1.ProposalRejection
		if err := proto.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.GetCode() != code {
			t.Fatalf("code = %v; want %v", decoded.GetCode(), code)
		}
	}
}

func TestImplementationRequestRejectsUntrustedRepositoryContext(t *testing.T) {
	var request reasoningv1.ImplementationRequest
	if err := proto.Unmarshal(implementationFixture(t, "request.bin"), &request); err != nil {
		t.Fatal(err)
	}
	request.RepositoryContext[0].Content = "tampered"
	if _, err := MapImplementationRequest(&request); err == nil {
		t.Fatal("repository context digest mismatch accepted")
	}
	if err := proto.Unmarshal(implementationFixture(t, "request.bin"), &request); err != nil {
		t.Fatal(err)
	}
	request.RepositoryContext[0].Path = "go/gen/secret.go"
	if _, err := MapImplementationRequest(&request); err == nil {
		t.Fatal("prohibited repository context accepted")
	}
	if err := proto.Unmarshal(implementationFixture(t, "request.bin"), &request); err != nil {
		t.Fatal(err)
	}
	request.Envelope.InputArtifacts = request.Envelope.InputArtifacts[:1]
	if _, err := MapImplementationRequest(&request); err == nil {
		t.Fatal("repository context without envelope artifact binding accepted")
	}
}
