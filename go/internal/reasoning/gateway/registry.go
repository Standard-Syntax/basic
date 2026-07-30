package gateway

import (
	"context"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/manifest"
)

// ManifestLookup adapts a digest lookup without allowing manifests to select
// provider identity, endpoint, or credentials.
type ManifestLookup func(context.Context, string) (manifest.Manifest, string, error)

func (f ManifestLookup) ResolveManifest(
	ctx context.Context, digest string,
) (ResolvedManifest, error) {
	value, actual, err := f(ctx, digest)
	return ResolvedManifest{Digest: actual, Manifest: value}, err
}

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }
