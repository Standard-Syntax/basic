package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"golang.org/x/sys/unix"
)

func TestValidateRequestEnforcesDecodedLimits(t *testing.T) {
	first := strings.Repeat("a", 4)
	second := strings.Repeat("b", 3)
	changes := []contracts.FileChange{
		{Path: "first", Operation: contracts.FileCreate, ReplacementContent: &first},
		{Path: "second", Operation: contracts.FileCreate, ReplacementContent: &second},
	}
	limits := execution.Limits{
		MaxChangedFiles: 2,
		MaxFileBytes:    4,
		MaxTotalBytes:   6,
		Timeout:         time.Minute,
	}
	if err := validateRequest(changes[:1], limits); err != nil {
		t.Fatalf("valid worker request rejected: %v", err)
	}
	if err := validateRequest(changes, limits); err == nil {
		t.Fatal("aggregate replacement limit was not enforced")
	}
	limits.MaxChangedFiles = 1
	if err := validateRequest(changes, limits); err == nil {
		t.Fatal("changed-file limit was not enforced")
	}
	limits.MaxChangedFiles = 2
	limits.MaxFileBytes = 3
	if err := validateRequest(changes[:1], limits); err == nil {
		t.Fatal("per-file limit was not enforced")
	}
}

func TestDecodeRequestUsesConfiguredEncodedLimit(t *testing.T) {
	content := "a"
	limits := execution.Limits{
		MaxChangedFiles: 1,
		MaxFileBytes:    1,
		MaxTotalBytes:   1,
		Timeout:         time.Minute,
	}
	valid, err := json.Marshal(workerInput{
		Changes: []contracts.FileChange{{
			Path: "file", Operation: contracts.FileCreate, ReplacementContent: &content,
		}},
		Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRequest(bytes.NewReader(valid)); err != nil {
		t.Fatalf("valid encoded request rejected: %v", err)
	}
	oversized, err := json.Marshal(workerInput{
		Changes: []contracts.FileChange{{
			Path: strings.Repeat("x", 1<<20), Operation: contracts.FileDelete,
		}},
		Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRequest(bytes.NewReader(oversized)); err == nil {
		t.Fatal("configured encoded-request limit was not enforced")
	}
}

func TestPreparedDirectoryDescriptorDefeatsSymlinkSwap(t *testing.T) {
	workspace := t.TempDir()
	authorized := filepath.Join(workspace, "authorized")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(authorized, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	original := []byte("original")
	if err := os.WriteFile(filepath.Join(authorized, "target"), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "target"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootFD, err := unix.Open(workspace, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFD)
	replacement := "replacement"
	sum := sha256.Sum256(original)
	item, err := prepare(rootFD, contracts.FileChange{
		Path: "authorized/target", Operation: contracts.FileUpdate,
		ExpectedOriginalSHA256: hex.EncodeToString(sum[:]),
		ReplacementContent:     &replacement,
	}, execution.DefaultMaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(item.dirFD)
	held := filepath.Join(workspace, "held")
	if err := os.Rename(authorized, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, authorized); err != nil {
		t.Fatal(err)
	}
	if err := apply(item); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(outside, "target")); err != nil ||
		string(body) != "outside" {
		t.Fatalf("symlink escape changed outside target: %q %v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(held, "target")); err != nil ||
		string(body) != replacement {
		t.Fatalf("descriptor-relative target mismatch: %q %v", body, err)
	}
}

func TestWorkerRejectsSymlinkLeafAndSpecialFile(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "regular"), []byte("value"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("regular", filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(workspace, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	rootFD, err := unix.Open(workspace, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFD)
	for _, target := range []string{"link", "fifo"} {
		if _, err := prepare(rootFD, contracts.FileChange{
			Path: target, Operation: contracts.FileDelete,
			ExpectedOriginalSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}, execution.DefaultMaxFileBytes); err == nil {
			t.Fatalf("unsafe target %q accepted", target)
		}
	}
}
