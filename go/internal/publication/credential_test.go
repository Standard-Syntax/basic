package publication

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
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
