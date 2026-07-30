// Command workflow-service runs the durable first-slice reconciler.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/approval"
	"github.com/Standard-Syntax/basic/go/internal/artifact"
	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"github.com/Standard-Syntax/basic/go/internal/orchestration"
	"github.com/Standard-Syntax/basic/go/internal/publication"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
	"github.com/Standard-Syntax/basic/go/internal/registry"
	"github.com/Standard-Syntax/basic/go/internal/review"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/stage"
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type config struct {
	DatabaseURL                string                 `json:"database_url"`
	ArtifactRoot               string                 `json:"artifact_root"`
	OwnerID                    string                 `json:"owner_id"`
	MaxArtifactBytes           int64                  `json:"max_artifact_bytes"`
	RepositoryRoot             string                 `json:"repository_root"`
	WorktreeRoot               string                 `json:"worktree_root"`
	VerificationWorkspaceRoot  string                 `json:"verification_workspace_root"`
	ExecutionWorkerImage       string                 `json:"execution_worker_image"`
	VerificationWorkerImage    string                 `json:"verification_worker_image"`
	WorkerUID                  int                    `json:"worker_uid"`
	WorkerGID                  int                    `json:"worker_gid"`
	ServiceActorID             string                 `json:"service_actor_id"`
	ReasoningActorID           string                 `json:"reasoning_actor_id"`
	ExecutionActorID           string                 `json:"execution_actor_id"`
	VerificationActorID        string                 `json:"verification_actor_id"`
	ReviewActorID              string                 `json:"review_actor_id"`
	ImplementationManifestPath string                 `json:"implementation_manifest_path"`
	ImplementationPromptPath   string                 `json:"implementation_prompt_path"`
	ReviewManifestPath         string                 `json:"review_manifest_path"`
	ReviewPromptPath           string                 `json:"review_prompt_path"`
	FakeImplementationProposal string                 `json:"fake_implementation_proposal_path"`
	FakeReviewProposal         string                 `json:"fake_review_proposal_path"`
	ContextMaxFiles            int                    `json:"context_max_files"`
	ContextMaxBytes            int64                  `json:"context_max_bytes"`
	TaskLeaseDuration          time.Duration          `json:"task_lease_duration_nanoseconds"`
	ClaimTTL                   time.Duration          `json:"claim_ttl_nanoseconds"`
	Provider                   gateway.ProviderConfig `json:"provider"`
}

func main() {
	os.Exit(mainExit())
}

func mainExit() int {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	configPath := flag.String("config", "", "absolute path to strict JSON configuration")
	flag.Parse()
	if *configPath == "" {
		slog.Error("configuration is required")
		return 2
	}
	value, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("load configuration", "error", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, value); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("workflow service stopped", "error", err)
		return 1
	}
	return 0
}

func loadConfig(path string) (config, error) {
	if !cleanAbsolutePath(path) {
		return config{}, errors.New("config path must be clean and absolute")
	}
	value, err := decodeConfig(path)
	if err != nil {
		return config{}, err
	}
	return normalizeConfig(value)
}

func decodeConfig(path string) (config, error) {
	file, err := os.Open(path)
	if err != nil {
		return config{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var value config
	if err := decoder.Decode(&value); err != nil {
		return config{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return config{}, errors.New("configuration has trailing content")
	}
	return value, nil
}

func normalizeConfig(value config) (config, error) {
	var err error
	value.Provider, err = value.Provider.Normalize()
	if err != nil {
		return config{}, err
	}
	if err := validateCleanAbsolutePaths("runtime", []string{
		value.ArtifactRoot, value.RepositoryRoot, value.WorktreeRoot,
		value.VerificationWorkspaceRoot, value.ImplementationManifestPath,
		value.ImplementationPromptPath, value.ReviewManifestPath, value.ReviewPromptPath,
	}); err != nil {
		return config{}, err
	}
	if value.Provider.Mode == gateway.FakeProviderMode {
		if err := validateCleanAbsolutePaths("fake proposal", []string{
			value.FakeImplementationProposal, value.FakeReviewProposal,
		}); err != nil {
			return config{}, err
		}
	}
	if !completeConfig(value) {
		return config{}, errors.New("incomplete configuration")
	}
	applyConfigDefaults(&value)
	return value, nil
}

func cleanAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func validateCleanAbsolutePaths(kind string, paths []string) error {
	for _, path := range paths {
		if !cleanAbsolutePath(path) {
			return fmt.Errorf("%s paths must be clean and absolute", kind)
		}
	}
	return nil
}

func completeConfig(value config) bool {
	required := []string{
		value.DatabaseURL, value.OwnerID, value.ServiceActorID, value.ReasoningActorID,
		value.ExecutionActorID, value.VerificationActorID, value.ReviewActorID,
		value.ExecutionWorkerImage, value.VerificationWorkerImage,
	}
	for _, item := range required {
		if item == "" {
			return false
		}
	}
	return value.WorkerUID > 0 && value.WorkerGID > 0
}

func applyConfigDefaults(value *config) {
	if value.ContextMaxFiles == 0 {
		value.ContextMaxFiles = 32
	}
	if value.ContextMaxBytes == 0 {
		value.ContextMaxBytes = 1 << 20
	}
	if value.TaskLeaseDuration == 0 {
		value.TaskLeaseDuration = 30 * time.Minute
	}
	if value.ClaimTTL == 0 {
		value.ClaimTTL = 30 * time.Second
	}
}

func run( // skipcq: GO-R1005 -- explicit fail-closed startup composition
	ctx context.Context, value config,
) error {
	migrateCtx, cancelMigrate := context.WithTimeout(ctx, 30*time.Second)
	err := migrateAll(migrateCtx, value.DatabaseURL)
	cancelMigrate()
	if err != nil {
		return fmt.Errorf("migrate workflow: %w", err)
	}
	pool, err := pgxpool.New(ctx, value.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	pingCtx, cancelPing := context.WithTimeout(ctx, 30*time.Second)
	err = pool.Ping(pingCtx)
	cancelPing()
	if err != nil {
		return err
	}
	artifacts, err := artifact.NewStore(value.ArtifactRoot, value.MaxArtifactBytes)
	if err != nil {
		return err
	}
	defer artifacts.Close()
	agentRegistry := registry.New(pool)
	implementationDigest, err := bootstrapManifest(
		ctx, artifacts, agentRegistry,
		value.ImplementationManifestPath, value.ImplementationPromptPath,
	)
	if err != nil {
		return fmt.Errorf("bootstrap implementation manifest: %w", err)
	}
	reviewDigest, err := bootstrapManifest(
		ctx, artifacts, agentRegistry, value.ReviewManifestPath, value.ReviewPromptPath,
	)
	if err != nil {
		return fmt.Errorf("bootstrap review manifest: %w", err)
	}
	resolver := gateway.ManifestLookup(func(
		ctx context.Context, digest string,
	) (manifest.Manifest, string, error) {
		record, err := agentRegistry.GetByDigest(ctx, digest)
		return record.Manifest, record.Digest, err
	})
	invocations, err := gateway.NewPostgresInvocationRepository(pool)
	if err != nil {
		return err
	}
	gatewayArtifacts := artifact.Gateway{Store: artifacts}
	implementationAdapter, reviewAdapter, err := providerAdapters(
		ctx, value, gatewayArtifacts,
	)
	if err != nil {
		return err
	}
	implementationGateway, err := gateway.NewService(
		resolver, implementationAdapter, gatewayArtifacts, invocations, gateway.SystemClock{},
	)
	if err != nil {
		return err
	}
	reviewGateway, err := gateway.NewReviewService(
		resolver, reviewAdapter, gatewayArtifacts, invocations, gateway.SystemClock{},
	)
	if err != nil {
		return err
	}
	workflowStore := workflow.NewStore(pool)
	executionService, err := execution.NewService(execution.Config{
		RepositoryRoot: value.RepositoryRoot, WorktreeRoot: value.WorktreeRoot,
		WorkerImage: value.ExecutionWorkerImage, UID: value.WorkerUID, GID: value.WorkerGID,
		ActorID: value.ExecutionActorID, AuthorName: "Harness Runtime",
		AuthorEmail: "harness@example.invalid",
	}, artifacts, execution.DockerApplicator{
		Image: value.ExecutionWorkerImage, UID: value.WorkerUID, GID: value.WorkerGID,
	}, workflowStore, execution.NewPostgresExecutionLedger(pool))
	if err != nil {
		return err
	}
	verificationService, err := verification.NewService(verification.Config{
		ActorID: value.VerificationActorID, Catalog: verification.DefaultCatalog(),
	}, artifacts, workflowStore, verification.FileWorkspacePreparer{
		RepositoryRoot: value.RepositoryRoot, WorkspaceRoot: value.VerificationWorkspaceRoot,
	}, verification.DockerCheckExecutor{
		Image: value.VerificationWorkerImage, UID: value.WorkerUID, GID: value.WorkerGID,
	}, verification.NewPostgresVerificationLedger(pool))
	if err != nil {
		return err
	}
	reviewService, err := review.NewService(
		review.Config{ActorID: value.ReviewActorID}, artifacts, reviewGateway, workflowStore,
	)
	if err != nil {
		return err
	}
	runtimeLedger := runtime.NewLedger(pool)
	bindings := runtime.NewBindingRepository(pool)
	stageHandlers, err := stage.New(stage.Config{
		RepositoryRoot: value.RepositoryRoot, ServiceActorID: value.ServiceActorID,
		ReasoningActorID:             value.ReasoningActorID,
		ImplementationManifestDigest: implementationDigest, ReviewManifestDigest: reviewDigest,
		TaskLeaseDuration: value.TaskLeaseDuration,
		ContextLimits: runtime.ContextLimits{
			MaxFiles: value.ContextMaxFiles, MaxBytes: value.ContextMaxBytes,
		},
		ImplementationBudget: runtime.ReasoningLimits{
			MaximumInputTokens: 100_000, MaximumOutputTokens: 20_000,
			MaximumProviderRequests: 1,
		},
		ReviewBudget: runtime.ReasoningLimits{
			MaximumInputTokens: 160_000, MaximumOutputTokens: 16_000,
			MaximumProviderRequests: 1,
		},
	}, artifacts, runtimePorts{Ledger: runtimeLedger, BindingRepository: bindings},
		workflowStore, implementationGateway, executionService, verificationService, reviewService)
	if err != nil {
		return err
	}
	reconciler, err := orchestration.New(orchestration.Config{
		OwnerID: value.OwnerID, ClaimTTL: value.ClaimTTL,
		PollInterval: 250 * time.Millisecond, MaxRetries: 5, InitialBackoff: time.Second,
	}, runtimeLedger, artifacts, stageHandlers.Map(), slog.Default())
	if err != nil {
		return err
	}
	slog.Info("workflow service ready", "owner_id", value.OwnerID)
	return reconciler.Run(ctx)
}

type runtimePorts struct {
	*runtime.Ledger
	*runtime.BindingRepository
}

func migrateAll(ctx context.Context, databaseURL string) error {
	for _, migrate := range []func(context.Context, string) error{
		workflow.Migrate, registry.Migrate, gateway.Migrate, execution.Migrate,
		verification.Migrate, approval.Migrate, publication.Migrate,
	} {
		if err := migrate(ctx, databaseURL); err != nil {
			return err
		}
	}
	return nil
}

func bootstrapManifest(
	ctx context.Context, artifacts *artifact.Store, agentRegistry *registry.Registry,
	manifestPath, promptPath string,
) (string, error) {
	rawManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", err
	}
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		return "", err
	}
	promptRef, err := artifacts.Put(ctx, prompt)
	if err != nil {
		return "", err
	}
	value, _, _, err := manifest.Read(rawManifest)
	if err != nil {
		return "", err
	}
	if value.Prompt.ArtifactURI != promptRef.URI || value.Prompt.SHA256 != promptRef.Digest {
		return "", errors.New("manifest prompt digest does not match configured prompt bytes")
	}
	record, err := agentRegistry.Register(ctx, rawManifest)
	if err != nil {
		return "", err
	}
	return record.Digest, nil
}

func providerAdapters(
	ctx context.Context, value config, artifacts gateway.ArtifactStore,
) (gateway.ImplementationAdapter, gateway.ReviewAdapter, error) {
	if value.Provider.Mode == gateway.FakeProviderMode {
		var implementation reasoningv1.ImplementationProposal
		if err := readProtoJSON(value.FakeImplementationProposal, &implementation); err != nil {
			return nil, nil, err
		}
		var review reasoningv1.ReviewProposal
		if err := readProtoJSON(value.FakeReviewProposal, &review); err != nil {
			return nil, nil, err
		}
		implementationAdapter, err := gateway.NewFakeImplementationAdapter(
			&implementation, "fake-implementation",
			gateway.Usage{InputTokens: 1, OutputTokens: 1, ProviderRequests: 1},
		)
		if err != nil {
			return nil, nil, err
		}
		reviewAdapter, err := gateway.NewFakeReviewAdapter(
			&review, "fake-review",
			gateway.Usage{InputTokens: 1, OutputTokens: 1, ProviderRequests: 1},
		)
		return implementationAdapter, reviewAdapter, err
	}
	credentials := gateway.EnvironmentCredentialSource{Name: value.Provider.APIKeyEnv}
	if _, err := credentials.Credential(ctx); err != nil {
		return nil, nil, err
	}
	models := gateway.MiniMaxModels()
	options := []gateway.AnthropicOption{
		gateway.WithAnthropicBaseURL(value.Provider.BaseURL),
		gateway.WithMiniMaxCompatibility(),
	}
	implementationAdapter, err := gateway.NewAnthropicImplementationAdapter(
		credentials, models, artifacts, options...,
	)
	if err != nil {
		return nil, nil, err
	}
	reviewAdapter, err := gateway.NewAnthropicReviewAdapter(
		credentials, models, artifacts, options...,
	)
	return implementationAdapter, reviewAdapter, err
}

func readProtoJSON(path string, destination proto.Message) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return protojson.UnmarshalOptions{DiscardUnknown: false}.Unmarshal(body, destination)
}
