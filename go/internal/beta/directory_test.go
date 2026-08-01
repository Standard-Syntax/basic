package beta

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestDirectoryDigestsAcceptsExactBoundaries(t *testing.T) {
	root := t.TempDir()
	large := strings.Repeat("x", MaxEvidenceFileSize)
	if err := os.WriteFile(filepath.Join(root, "file-00"), []byte(large), 0o600); err != nil {
		t.Fatal(err)
	}
	for index := 1; index < MaxEvidenceFiles; index++ {
		name := filepath.Join(root, "file-"+string(rune('A'+index)))
		if err := os.WriteFile(name, []byte{byte(index)}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	values, err := DirectoryDigests(root)
	if err != nil || len(values) != MaxEvidenceFiles {
		t.Fatalf("digests = %d, %v", len(values), err)
	}
	want, err := digestFile(filepath.Join(root, "file-00"))
	if err != nil || values["file-00"] != want {
		t.Fatalf("boundary digest = %q, %v", values["file-00"], err)
	}
}

func TestDirectoryDigestsRejectsUnsafeInputs(t *testing.T) {
	tests := []struct {
		name string
		set  func(*testing.T, string)
		want string
	}{
		{"oversized", func(t *testing.T, root string) {
			body := make([]byte, MaxEvidenceFileSize+1)
			if err := os.WriteFile(filepath.Join(root, "large"), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "size limit"},
		{"excess", func(t *testing.T, root string) {
			for index := 0; index <= MaxEvidenceFiles; index++ {
				name := filepath.Join(root, strings.Repeat("x", index/26+1)+string(rune('a'+index%26)))
				if err := os.WriteFile(name, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}, "file limit"},
		{"symlink", func(t *testing.T, root string) {
			target := filepath.Join(root, "target")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
				t.Fatal(err)
			}
		}, "symlink"},
		{"non_regular", func(t *testing.T, root string) {
			if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "non-regular"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			test.set(t, root)
			_, err := DirectoryDigests(root)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDirectoryDigestsUsesRelativeSlashPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "value"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := DirectoryDigests(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"nested/value": "cd42404d52ad55ccfa9aca4adc828aa5800ad9d385a0671fbcbf724118320619"}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("digests = %#v", values)
	}
}
