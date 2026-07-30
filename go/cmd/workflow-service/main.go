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

	"github.com/Standard-Syntax/basic/go/internal/artifact"
	"github.com/Standard-Syntax/basic/go/internal/orchestration"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/jackc/pgx/v5/pgxpool"
)

type config struct {
	DatabaseURL      string `json:"database_url"`
	ArtifactRoot     string `json:"artifact_root"`
	OwnerID          string `json:"owner_id"`
	MaxArtifactBytes int64  `json:"max_artifact_bytes"`
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
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return config{}, errors.New("config path must be clean and absolute")
	}
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
	if value.DatabaseURL == "" || !filepath.IsAbs(value.ArtifactRoot) ||
		filepath.Clean(value.ArtifactRoot) != value.ArtifactRoot || value.OwnerID == "" {
		return config{}, errors.New("incomplete configuration")
	}
	return value, nil
}

func run(ctx context.Context, value config) error {
	migrateCtx, cancelMigrate := context.WithTimeout(ctx, 30*time.Second)
	err := workflow.Migrate(migrateCtx, value.DatabaseURL)
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
	handlers := make(map[string]orchestration.Handler)
	for _, stage := range []string{
		orchestration.StageStart, orchestration.StageImplementationRequest,
		orchestration.StageReasoning, orchestration.StageExecution,
		orchestration.StageVerification, orchestration.StageReview,
		orchestration.StageAwaitingApproval,
	} {
		current := stage
		handlers[stage] = orchestration.HandlerFunc(func(
			ctx context.Context, job runtime.Job, ids orchestration.Identities,
		) (workflow.ArtifactRef, error) {
			body, err := json.Marshal(struct {
				SchemaVersion string                   `json:"schema_version"`
				Stage         string                   `json:"stage"`
				JobID         string                   `json:"job_id"`
				Identities    orchestration.Identities `json:"identities"`
			}{"runtime_checkpoint.v1", current, job.ID, ids})
			if err != nil {
				return workflow.ArtifactRef{}, err
			}
			return artifacts.Put(ctx, body)
		})
	}
	reconciler, err := orchestration.New(orchestration.Config{
		OwnerID: value.OwnerID, ClaimTTL: 30 * time.Second,
		PollInterval: 250 * time.Millisecond, MaxRetries: 5, InitialBackoff: time.Second,
	}, runtime.NewLedger(pool), artifacts, handlers, slog.Default())
	if err != nil {
		return err
	}
	slog.Info("workflow service ready", "owner_id", value.OwnerID)
	return reconciler.Run(ctx)
}
