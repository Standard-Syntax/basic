package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

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
