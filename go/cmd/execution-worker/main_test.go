package main

import (
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
