package publication

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestFileCredentialReadsPerRequestAndChecksPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := NewFileCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	if token, err := source.Token(context.Background()); err != nil || token != "first" {
		t.Fatalf("first token = %q, %v", token, err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err := source.Token(context.Background()); err != nil || token != "second" {
		t.Fatalf("second token = %q, %v", token, err)
	}
	// Deliberately create an unsafe credential to prove the reader rejects it.
	if err := os.Chmod(path, 0o640); err != nil { // skipcq: GSC-G302
		t.Fatal(err)
	}
	if _, err := source.Token(context.Background()); !errors.Is(err, ErrCredentialPermissions) {
		t.Fatalf("unsafe permissions = %v", err)
	}
}

func TestFileCredentialRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target, link := filepath.Join(root, "target"), filepath.Join(root, "link")
	if err := os.WriteFile(target, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileCredential(link); !errors.Is(err, ErrCredentialPermissions) {
		t.Fatalf("symlink = %v", err)
	}
}

func TestCredentialReaderHandlesShortReadsAndRejectsOversizeValues(t *testing.T) {
	value := strings.Repeat("a", 4096)
	token, err := readCredential(iotest.OneByteReader(strings.NewReader(value)))
	if err != nil || token != value {
		t.Fatalf("chunked token length=%d err=%v", len(token), err)
	}
	if _, err := readCredential(strings.NewReader(value + "b")); !errors.Is(err, ErrCredentialPermissions) {
		t.Fatalf("4097-byte credential error=%v", err)
	}
	for _, invalid := range []string{"", "first\nsecond", "first\rsecond"} {
		if _, err := readCredential(strings.NewReader(invalid)); !errors.Is(err, ErrCredentialPermissions) {
			t.Fatalf("invalid credential %q error=%v", invalid, err)
		}
	}
}
