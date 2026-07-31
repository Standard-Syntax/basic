package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
)

func validTestConfig() config {
	return config{
		DatabaseURL:  "postgres://unused.invalid/test",
		ArtifactRoot: "/tmp/basic-artifacts", OwnerID: "owner",
		RepositoryRoot: "/tmp/basic-repository", WorktreeRoot: "/tmp/basic-worktrees",
		VerificationWorkspaceRoot: "/tmp/basic-verification",
		ExecutionWorkerImage:      "execution", VerificationWorkerImage: "verification",
		WorkerUID: 1, WorkerGID: 1, ServiceActorID: "service",
		ReasoningActorID: "reasoning", ExecutionActorID: "execution",
		VerificationActorID: "verification", ReviewActorID: "review",
		ImplementationManifestPath: "/tmp/implementation.json",
		ImplementationPromptPath:   "/tmp/implementation.md",
		ReviewManifestPath:         "/tmp/review.json", ReviewPromptPath: "/tmp/review.md",
	}
}

func TestWorkflowProviderDefaultsAreClosedMiniMaxProfile(t *testing.T) {
	value, err := normalizeConfig(validTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	want := gateway.ProviderConfig{
		Mode: gateway.MiniMaxMode, BaseURL: gateway.MiniMaxBaseURL,
		Model: gateway.MiniMaxModel, APIKeyEnv: gateway.MiniMaxAPIKeyEnv,
	}
	if value.Provider != want {
		t.Fatalf("provider = %#v; want %#v", value.Provider, want)
	}
}

func TestWorkflowConfigurationRejectsRemovedAndUnknownFields(t *testing.T) {
	base, err := json.Marshal(validTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(base, &values); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"fake_implementation_proposal_path",
		"fake_review_proposal_path",
		"unknown",
	} {
		t.Run(field, func(t *testing.T) {
			copy := make(map[string]any, len(values)+1)
			for key, value := range values {
				copy[key] = value
			}
			copy[field] = "/tmp/removed.json"
			path := writeTestConfig(t, copy, "")
			if _, err := loadConfig(path); err == nil ||
				!strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("removed field %q error = %v", field, err)
			}
		})
	}
}

func TestWorkflowConfigurationRejectsFakeAndTrailingJSON(t *testing.T) {
	value := validTestConfig()
	value.Provider.Mode = "fake"
	if _, err := normalizeConfig(value); err == nil {
		t.Fatal("fake provider mode was accepted")
	}
	path := writeTestConfig(t, validTestConfig(), `{}`)
	if _, err := loadConfig(path); err == nil ||
		!strings.Contains(err.Error(), "trailing content") {
		t.Fatalf("trailing JSON error = %v", err)
	}
}

func TestMissingCredentialFailsBeforeDatabaseOrOrchestration(t *testing.T) {
	if err := os.Unsetenv(gateway.MiniMaxAPIKeyEnv); err != nil {
		t.Fatal(err)
	}
	value, err := normalizeConfig(validTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	value.DatabaseURL = "not a database URL"
	err = run(context.Background(), value)
	if !errors.Is(err, gateway.ErrCredentialUnavailable) {
		t.Fatalf("startup error = %v", err)
	}
	if strings.Contains(err.Error(), gateway.MiniMaxAPIKeyEnv) {
		t.Fatalf("startup error disclosed credential configuration: %v", err)
	}
}

func writeTestConfig(t *testing.T, value any, trailing string) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "workflow.json")
	if err := os.WriteFile(path, append(body, trailing...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
