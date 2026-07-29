// Package artifact provides the durable, local content-addressed artifact store.
package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"golang.org/x/sys/unix"
)

var (
	ErrInvalidRoot = errors.New("artifact root must be an absolute directory")
	ErrInvalidRef  = errors.New("invalid artifact reference")
	ErrTooLarge    = errors.New("artifact exceeds configured limit")
	ErrIntegrity   = errors.New("artifact integrity check failed")
	ErrUnsafe      = errors.New("unsafe artifact path")
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

const DefaultMaxBytes int64 = 1 << 20

type Store struct {
	root     string
	maxBytes int64
}

func NewStore(root string, maxBytes int64) (*Store, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrInvalidRoot
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalidRoot
	}
	return &Store{root: root, maxBytes: maxBytes}, nil
}

func (s *Store) Put(ctx context.Context, body []byte) (workflow.ArtifactRef, error) {
	if err := ctx.Err(); err != nil {
		return workflow.ArtifactRef{}, err
	}
	if int64(len(body)) > s.maxBytes {
		return workflow.ArtifactRef{}, ErrTooLarge
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	ref := workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}
	dir := filepath.Join(s.root, digest[:2])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return workflow.ArtifactRef{}, fmt.Errorf("create artifact shard: %w", err)
	}
	if err := rejectLink(dir); err != nil {
		return workflow.ArtifactRef{}, err
	}
	target := filepath.Join(dir, digest)
	if existing, err := s.readPath(ctx, target, digest, s.maxBytes); err == nil {
		if !bytes.Equal(existing, body) {
			return workflow.ArtifactRef{}, ErrIntegrity
		}
		return ref, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return workflow.ArtifactRef{}, err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return workflow.ArtifactRef{}, fmt.Errorf("create artifact temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return workflow.ArtifactRef{}, err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return workflow.ArtifactRef{}, fmt.Errorf("write artifact: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return workflow.ArtifactRef{}, fmt.Errorf("sync artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return workflow.ArtifactRef{}, err
	}
	if err := os.Link(tmpName, target); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return workflow.ArtifactRef{}, fmt.Errorf("publish artifact: %w", err)
		}
		existing, readErr := s.readPath(ctx, target, digest, s.maxBytes)
		if readErr != nil || !bytes.Equal(existing, body) {
			return workflow.ArtifactRef{}, ErrIntegrity
		}
	}
	directory, err := os.Open(dir)
	if err != nil {
		return workflow.ArtifactRef{}, err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return workflow.ArtifactRef{}, fmt.Errorf("sync artifact directory: %w", err)
	}
	return ref, nil
}

func (s *Store) Get(ctx context.Context, ref workflow.ArtifactRef) ([]byte, error) {
	return s.GetLimit(ctx, ref, s.maxBytes)
}

func (s *Store) GetLimit(ctx context.Context, ref workflow.ArtifactRef, limit int64) ([]byte, error) {
	if ref.URI != "artifact://sha256/"+ref.Digest || !digestPattern.MatchString(ref.Digest) {
		return nil, ErrInvalidRef
	}
	if limit <= 0 || limit > s.maxBytes {
		return nil, ErrTooLarge
	}
	return s.readPath(ctx, filepath.Join(s.root, ref.Digest[:2], ref.Digest), ref.Digest, limit)
}

func (s *Store) readPath(ctx context.Context, path, digest string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := rejectLink(filepath.Dir(path)); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, ErrUnsafe
		}
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrUnsafe
	}
	if info.Size() > limit {
		return nil, ErrTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, ErrTooLarge
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, ErrIntegrity
	}
	return body, nil
}

func rejectLink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafe
	}
	return nil
}

// Publication adapts Store to publication's caller-selected read limit.
type Publication struct{ Store *Store }

func (a Publication) Put(ctx context.Context, body []byte) (workflow.ArtifactRef, error) {
	return a.Store.Put(ctx, body)
}
func (a Publication) Get(ctx context.Context, ref workflow.ArtifactRef, limit int64) ([]byte, error) {
	return a.Store.GetLimit(ctx, ref, limit)
}

// Gateway adapts Store to the reasoning gateway's reference type.
type Gateway struct{ Store *Store }

func (a Gateway) Put(ctx context.Context, body []byte) (gateway.ArtifactReference, error) {
	ref, err := a.Store.Put(ctx, body)
	return gateway.ArtifactReference{URI: ref.URI, SHA256: ref.Digest}, err
}
func (a Gateway) Get(ctx context.Context, ref gateway.ArtifactReference) ([]byte, error) {
	return a.Store.Get(ctx, workflow.ArtifactRef{URI: ref.URI, Digest: ref.SHA256})
}
