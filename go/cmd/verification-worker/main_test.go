package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerRejectsUnapprovedCommand(t *testing.T) {
	input := `{"command_reference":"model-command","argv":["sh","-c","true"],` +
		`"timeout_nanoseconds":1000000000,"output_bytes":1024}`
	var output bytes.Buffer
	if err := run(strings.NewReader(input), &output); err == nil {
		t.Fatal("unapproved command executed")
	}
}

func TestSecureRuntimeCacheUsesLeastPrivilegeModes(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "nested")
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "cache-entry")
	if err := os.WriteFile(file, []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/cache-entry", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := secureRuntimeCache(root); err != nil {
		t.Fatal(err)
	}
	assertMode(t, root, 0o700)
	assertMode(t, directory, 0o700)
	assertMode(t, file, 0o600)
}

func TestSecureRuntimeCacheRejectsSymbolicLinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := secureRuntimeCache(root); err == nil {
		t.Fatal("symbolic link accepted in runtime cache")
	}
}

func assertMode(t *testing.T, path string, expected os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != expected {
		t.Fatalf("%s mode = %04o, want %04o", path, mode, expected)
	}
}

func TestWorkerRejectsUnknownFieldsAndTrailers(t *testing.T) {
	cases := []string{
		`{"command_reference":"make-check-v1","argv":["make","check"],` +
			`"timeout_nanoseconds":1000000000,"output_bytes":1024,"extra":true}`,
		`{"command_reference":"make-check-v1","argv":["make","check"],` +
			`"timeout_nanoseconds":1000000000,"output_bytes":1024}{}`,
	}
	for _, input := range cases {
		if _, err := decode(strings.NewReader(input)); err == nil {
			t.Fatalf("invalid input accepted: %s", input)
		}
	}
}

func TestWorkerBoundedBuffer(t *testing.T) {
	buffer := boundedBuffer{limit: 3}
	_, _ = buffer.Write([]byte("abcdef"))
	if got := buffer.buffer.String(); got != "abc" || !buffer.overflow {
		t.Fatalf("buffer = %q overflow=%v", got, buffer.overflow)
	}
}

func TestSecureRuntimeCacheUsesLeastPrivilegeModes(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "nested")
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "cache-entry")
	if err := os.WriteFile(file, []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/cache-entry", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := secureRuntimeCache(root); err != nil {
		t.Fatal(err)
	}
	assertMode(t, root, 0o700)
	assertMode(t, directory, 0o700)
	assertMode(t, file, 0o600)
}

func TestSecureRuntimeCacheRejectsSymbolicLinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := secureRuntimeCache(root); err == nil {
		t.Fatal("symbolic link accepted in runtime cache")
	}
}

func assertMode(t *testing.T, path string, expected os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != expected {
		t.Fatalf("%s mode = %04o, want %04o", path, mode, expected)
	}
}
