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
	"strings"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
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
var temporaryPattern = regexp.MustCompile(`^\.tmp-[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

const DefaultMaxBytes int64 = 1 << 20

type Store struct {
	root     *os.File
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
	store, err := OpenStore(root, maxBytes)
	if err != nil {
		return nil, err
	}
	if err := store.sweepTemporaryFiles(time.Now().UTC().Add(-24 * time.Hour)); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) sweepTemporaryFiles(olderThan time.Time) error {
	rootFD, err := unix.Openat(
		int(s.root.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0,
	)
	if err != nil {
		return err
	}
	root := os.NewFile(uintptr(rootFD), "artifact-root")
	defer root.Close()
	shards, err := root.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range shards {
		if !validShardName(name) {
			continue
		}
		if err := s.sweepShard(name, olderThan); err != nil {
			return err
		}
	}
	return nil
}

func validShardName(name string) bool {
	return len(name) == 2 && strings.Trim(name, "0123456789abcdef") == ""
}

func (s *Store) sweepShard(name string, olderThan time.Time) error {
	shard, err := s.openShard(name, false)
	if errors.Is(err, ErrUnsafe) || errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	names, err := shard.Readdirnames(-1)
	if err != nil {
		_ = shard.Close()
		return err
	}
	for _, candidate := range names {
		if err := sweepTemporaryAt(shard, candidate, olderThan); err != nil {
			_ = shard.Close()
			return err
		}
	}
	return shard.Close()
}

func sweepTemporaryAt(shard *os.File, candidate string, olderThan time.Time) error {
	if !temporaryPattern.MatchString(candidate) {
		return nil
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(shard.Fd()), candidate, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	modified := time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || !modified.Before(olderThan) {
		return nil
	}
	if err := unix.Unlinkat(int(shard.Fd()), candidate, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// OpenStore opens an existing artifact root without creating or modifying it.
// Read-only evidence verifiers use this boundary so a readiness check cannot
// turn a missing durable store into an empty one.
func OpenStore(root string, maxBytes int64) (*Store, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrInvalidRoot
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalidRoot
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrInvalidRoot
	}
	return &Store{root: os.NewFile(uintptr(fd), root), maxBytes: maxBytes}, nil
}

func (s *Store) Close() error { return s.root.Close() }

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
	shard, err := s.openShard(digest[:2], true)
	if err != nil {
		return workflow.ArtifactRef{}, err
	}
	defer shard.Close()
	if exists, err := matchesAt(ctx, int(shard.Fd()), digest, body, s.maxBytes); err != nil {
		return workflow.ArtifactRef{}, err
	} else if exists {
		return ref, nil
	}
	tmpName, err := writeTemporaryAt(int(shard.Fd()), body)
	if err != nil {
		return workflow.ArtifactRef{}, err
	}
	defer func() { _ = unix.Unlinkat(int(shard.Fd()), tmpName, 0) }()
	if err := publishAt(ctx, shard, tmpName, digest, body, s.maxBytes); err != nil {
		return workflow.ArtifactRef{}, err
	}
	return ref, nil
}

func matchesAt(
	ctx context.Context, directory int, digest string, body []byte, limit int64,
) (bool, error) {
	existing, err := readAt(ctx, directory, digest, digest, limit)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !bytes.Equal(existing, body) {
		return false, ErrIntegrity
	}
	return true, nil
}

func writeTemporaryAt(directory int, body []byte) (string, error) {
	tmpName := ".tmp-" + uuid.NewString()
	tmpFD, err := unix.Openat(
		directory, tmpName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0o600,
	)
	if err != nil {
		return "", fmt.Errorf("create artifact temporary file: %w", err)
	}
	tmp := os.NewFile(uintptr(tmpFD), tmpName)
	cleanup := func() { _ = unix.Unlinkat(directory, tmpName, 0) }
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("write artifact: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("sync artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", err
	}
	return tmpName, nil
}

func publishAt(
	ctx context.Context, shard *os.File, tmpName, digest string, body []byte, limit int64,
) error {
	if err := unix.Linkat(int(shard.Fd()), tmpName, int(shard.Fd()), digest, 0); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("publish artifact: %w", err)
		}
		existing, readErr := readAt(ctx, int(shard.Fd()), digest, digest, limit)
		if readErr != nil || !bytes.Equal(existing, body) {
			return ErrIntegrity
		}
	}
	if err := shard.Sync(); err != nil {
		return fmt.Errorf("sync artifact directory: %w", err)
	}
	return nil
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
	shard, err := s.openShard(ref.Digest[:2], false)
	if err != nil {
		return nil, err
	}
	defer shard.Close()
	return readAt(ctx, int(shard.Fd()), ref.Digest, ref.Digest, limit)
}

func (s *Store) openShard(name string, create bool) (*os.File, error) {
	if create {
		err := unix.Mkdirat(int(s.root.Fd()), name, 0o700)
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("create artifact shard: %w", err)
		}
	}
	fd, err := unix.Openat(
		int(s.root.Fd()), name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, ErrUnsafe
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func readAt(ctx context.Context, directory int, name, digest string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(
		directory, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0,
	)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, ErrUnsafe
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
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
