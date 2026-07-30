package publication

import (
	"context"
	"errors"
	"testing"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"google.golang.org/protobuf/proto"
)

type boundedArtifactStore struct {
	value          []byte
	requestedLimit int64
}

func (s *boundedArtifactStore) Get(
	_ context.Context, _ workflow.ArtifactRef, limit int64,
) ([]byte, error) {
	s.requestedLimit = limit
	if int64(len(s.value)) > limit {
		return nil, ErrResponseLimit
	}
	return append([]byte(nil), s.value...), nil
}

func (*boundedArtifactStore) Put(
	context.Context, []byte,
) (workflow.ArtifactRef, error) {
	return workflow.ArtifactRef{}, errors.New("not implemented")
}

func TestLoadArtifactDelegatesLimitBeforeMaterialization(t *testing.T) {
	store := &boundedArtifactStore{value: []byte("oversized")}
	const limit int64 = 4
	_, err := loadArtifact(
		t.Context(), store,
		workflow.ArtifactRef{
			URI:    "artifact://publication/oversized",
			Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		limit,
	)
	if !errors.Is(err, ErrResponseLimit) {
		t.Fatalf("load artifact error = %v", err)
	}
	if store.requestedLimit != limit {
		t.Fatalf("requested limit = %d", store.requestedLimit)
	}
}

func TestSpecificationArtifactAcceptsDeterministicProtobuf(t *testing.T) {
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(
		&reasoningv1.SpecificationProposal{Title: "Approved protobuf specification"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !validSpecificationArtifact(body) {
		t.Fatal("deterministic Protobuf specification artifact rejected")
	}
	if title := specificationTitle(body); title != "Approved protobuf specification" {
		t.Fatalf("specification title = %q", title)
	}
}
