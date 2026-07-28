package contracts

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"google.golang.org/protobuf/proto"
)

func reviewFixture(t *testing.T, name string) []byte {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	path := filepath.Join(
		filepath.Dir(source), "..", "..", "..", "..", "tests", "contracts", "v1", "review", name,
	)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func validReview(t *testing.T) (ReviewRequest, *reasoningv1.ReviewProposal) {
	t.Helper()
	var requestPB reasoningv1.ReviewRequest
	if err := proto.Unmarshal(reviewFixture(t, "request.bin"), &requestPB); err != nil {
		t.Fatal(err)
	}
	request, err := MapReviewRequest(&requestPB)
	if err != nil {
		t.Fatal(err)
	}
	var proposal reasoningv1.ReviewProposal
	if err := proto.Unmarshal(reviewFixture(t, "proposal.bin"), &proposal); err != nil {
		t.Fatal(err)
	}
	return request, &proposal
}

func TestReviewFixturesMapAndRoundTripInGo(t *testing.T) {
	request, proposal := validReview(t)
	mapped, err := MapReviewProposal(proposal, request)
	if err != nil {
		t.Fatal(err)
	}
	if mapped.Recommendation != ReviewAdvisoryAccept {
		t.Fatalf("recommendation = %q", mapped.Recommendation)
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(reviewFixture(t, "proposal.bin")) {
		t.Fatal("Go deterministic serialization differs from Python fixture")
	}
}

func TestReviewRecommendationCannotRepresentApproval(t *testing.T) {
	descriptor := (&reasoningv1.ReviewProposal{}).ProtoReflect().Descriptor()
	for index := 0; index < descriptor.Fields().Len(); index++ {
		if strings.Contains(string(descriptor.Fields().Get(index).Name()), "approval") {
			t.Fatal("review proposal contains an approval field")
		}
	}
}

func TestReviewRejectsEvidenceBoundToAnotherCandidate(t *testing.T) {
	var requestPB reasoningv1.ReviewRequest
	if err := proto.Unmarshal(reviewFixture(t, "request.bin"), &requestPB); err != nil {
		t.Fatal(err)
	}
	requestPB.IndependentEvidence[0].CandidateCommit = strings.Repeat("1", 40)
	if _, err := MapReviewRequest(&requestPB); err == nil {
		t.Fatal("mismatched evidence candidate accepted")
	}
}

func TestGoPreservesUnknownProtobufFields(t *testing.T) {
	original := reviewFixture(t, "proposal.bin")
	withUnknown := append(append([]byte{}, original...), 0xf8, 0x07, 0x01)
	var proposal reasoningv1.ReviewProposal
	if err := proto.Unmarshal(withUnknown, &proposal); err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(&proposal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(encoded), string([]byte{0xf8, 0x07, 0x01})) {
		t.Fatal("unknown field was not preserved")
	}
}
