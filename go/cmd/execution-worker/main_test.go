package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
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
