package repository

import (
	"errors"
	"testing"
)

func TestParseTreeRecordRejectsUnsafePathsAndAcceptsSymlinkMode(t *testing.T) {
	object := "0123456789012345678901234567890123456789"
	for _, name := range []string{"/absolute", "../escape", "dir/../escape", ".git/config", "dir\\file", "control\nfile"} {
		record := []byte("100644 blob " + object + "\t" + name)
		if _, _, _, err := parseTreeRecord(record); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("path %q error = %v", name, err)
		}
	}
	mode, _, name, err := parseTreeRecord([]byte("120000 blob " + object + "\tdocs/link"))
	if err != nil || mode != "120000" || name != "docs/link" {
		t.Fatalf("symlink record = %q %q %v", mode, name, err)
	}
}
