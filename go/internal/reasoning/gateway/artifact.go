package gateway

import (
	"context"
	"errors"
)

var ErrArtifactIntegrity = errors.New("artifact integrity check failed")

type ArtifactStore interface {
	Put(context.Context, []byte) (ArtifactReference, error)
	Get(context.Context, ArtifactReference) ([]byte, error)
}
