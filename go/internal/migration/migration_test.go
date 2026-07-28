package migration

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestReadAllSortsByParsedVersion(t *testing.T) {
	files := fstest.MapFS{
		"migrations/10_tenth.sql":  {Data: []byte("SELECT 10;")},
		"migrations/2_second.sql":  {Data: []byte("SELECT 2;")},
		"migrations/001_first.sql": {Data: []byte("SELECT 1;")},
		"migrations/README.md":     {Data: []byte("ignored")},
	}
	items, err := readAll(files, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("migration count = %d", len(items))
	}
	for index, want := range []int64{1, 2, 10} {
		if items[index].version != want {
			t.Fatalf("migration[%d].version = %d; want %d", index, items[index].version, want)
		}
	}
}

func TestReadAllRejectsDuplicateParsedVersions(t *testing.T) {
	files := fstest.MapFS{
		"migrations/1_first.sql":  {Data: []byte("SELECT 1;")},
		"migrations/01_alias.sql": {Data: []byte("SELECT 2;")},
	}
	_, err := readAll(files, "migrations")
	if err == nil || !strings.Contains(err.Error(), "duplicate migration version 1") {
		t.Fatalf("readAll error = %v", err)
	}
}
