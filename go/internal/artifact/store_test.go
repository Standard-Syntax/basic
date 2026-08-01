package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
)

func TestStoreConcurrentIdempotentPutAndBoundedGet(t *testing.T) {
	store, err := NewStore(t.TempDir(), 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	body := []byte("durable")
	var wg sync.WaitGroup
	refs := make(chan workflow.ArtifactRef, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ref, putErr := store.Put(context.Background(), body)
			if putErr != nil {
				t.Errorf("put: %v", putErr)
				return
			}
			refs <- ref
		}()
	}
	wg.Wait()
	close(refs)
	var expected workflow.ArtifactRef
	for ref := range refs {
		if expected.URI == "" {
			expected = ref
		}
		if ref != expected {
			t.Fatalf("non-deterministic reference: %#v != %#v", ref, expected)
		}
	}
	got, err := store.Get(context.Background(), expected)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("get = %q, %v", got, err)
	}
	if _, err := store.GetLimit(context.Background(), expected, 3); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("bounded get = %v", err)
	}
	if _, err := store.Put(context.Background(), bytes.Repeat([]byte("x"), 33)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized put = %v", err)
	}
}

func TestStoreRejectsCorruptionSymlinkAndCancellation(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root, 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ref, err := store.Put(context.Background(), []byte("original"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ref.Digest[:2], ref.Digest)
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), ref); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corruption = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), ref); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("symlink = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Put(ctx, []byte("later")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled put = %v", err)
	}
}

func TestStoreRequiresCleanAbsoluteRoot(t *testing.T) {
	if _, err := NewStore("relative", 1); !errors.Is(err, ErrInvalidRoot) {
		t.Fatalf("relative root = %v", err)
	}
}

func TestOpenStoreDoesNotCreateMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	if _, err := OpenStore(root, DefaultMaxBytes); err == nil {
		t.Fatal("missing artifact root was opened")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only open created the artifact root: %v", err)
	}
}

func TestWritableOpenSweepsOnlyOldRegularTemporaryFiles(t *testing.T) {
	root := t.TempDir()
	shard := filepath.Join(root, "ab")
	if err := os.Mkdir(shard, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(shard, ".tmp-11111111-1111-4111-8111-111111111111")
	recent := filepath.Join(shard, ".tmp-22222222-2222-4222-8222-222222222222")
	nonconforming := filepath.Join(shard, ".tmp-not-a-uuid")
	symlink := filepath.Join(shard, ".tmp-33333333-3333-4333-8333-333333333333")
	for _, path := range []string{old, recent, nonconforming} {
		if err := os.WriteFile(path, []byte("temporary"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(old, symlink); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-25 * time.Hour)
	for _, path := range []string{old, nonconforming, symlink} {
		if err := os.Chtimes(path, stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	readOnly, err := OpenStore(root, DefaultMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(old); err != nil {
		t.Fatalf("read-only open swept file: %v", err)
	}
	writable, err := NewStore(root, DefaultMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer writable.Close()
	if _, err := os.Lstat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old temporary file remains: %v", err)
	}
	for _, path := range []string{recent, nonconforming, symlink} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("preserved %s: %v", path, err)
		}
	}
}
