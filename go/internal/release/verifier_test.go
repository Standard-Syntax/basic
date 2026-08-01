package release

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStrictJSONRejectsUnknownFieldsAndTrailers(t *testing.T) {
	type value struct {
		Name string `json:"name"`
	}
	var decoded value
	if err := strictJSON([]byte(`{"name":"ready"}`), &decoded); err != nil || decoded.Name != "ready" {
		t.Fatalf("strict decode = %#v, %v", decoded, err)
	}
	for _, body := range []string{`{"name":"ready","unknown":true}`, `{"name":"ready"} {}`} {
		if err := strictJSON([]byte(body), &value{}); err == nil {
			t.Fatalf("invalid JSON was accepted: %s", body)
		}
	}
}

func TestDirectoryDigestsAreRelativeAndSymlinkFree(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "b"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := directoryDigests(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": digest([]byte("a")), "nested/b": digest([]byte("b"))}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("digests = %#v, want %#v", values, want)
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := directoryDigests(root); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}
