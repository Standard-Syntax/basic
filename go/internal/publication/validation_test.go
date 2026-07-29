package publication

import (
	"context"
	"errors"
	"testing"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
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
