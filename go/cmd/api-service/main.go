// Command api-service runs the authenticated user-facing control plane.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/approval"
	"github.com/Standard-Syntax/basic/go/internal/artifact"
	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/controlapi"
	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/migration"
	"github.com/Standard-Syntax/basic/go/internal/publication"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
	"github.com/Standard-Syntax/basic/go/internal/registry"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/jackc/pgx/v5/pgxpool"
)

type config struct {
	Listen           string                 `json:"listen"`
	DatabaseURL      string                 `json:"database_url"`
	ArtifactRoot     string                 `json:"artifact_root"`
	RepositoryRoot   string                 `json:"repository_root"`
	ServiceActorID   string                 `json:"service_actor_id"`
	MaxArtifactBytes int64                  `json:"max_artifact_bytes"`
	MaxBodyBytes     int64                  `json:"max_body_bytes"`
	TaskMaxAttempts  uint32                 `json:"task_max_attempts,omitempty"`
	TrustedChecks    []string               `json:"trusted_checks"`
	Principals       []controlapi.Principal `json:"principals"`
	Publication      *publicationConfig     `json:"publication,omitempty"`
	Policy           beta.Policy            `json:"beta_policy"`
}

type publicationConfig struct {
	RepositoryRoot        string `json:"repository_root"`
	RepositoryOwner       string `json:"repository_owner"`
	RepositoryName        string `json:"repository_name"`
	Remote                string `json:"remote"`
	BaseBranch            string `json:"base_branch"`
	BranchPrefix          string `json:"branch_prefix"`
	ActorID               string `json:"actor_id"`
	APIEndpoint           string `json:"api_endpoint"`
	APIVersion            string `json:"api_version"`
	TokenFile             string `json:"token_file"`
	GitPushCredentialFile string `json:"git_push_credential_file,omitempty"`
}

func main() {
	os.Exit(mainExit())
}

func mainExit() int {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	configPath := flag.String("config", "", "absolute path to strict JSON configuration")
	healthcheck := flag.String("healthcheck", "", "probe one local health endpoint")
	flag.Parse()
	if *healthcheck != "" {
		return probeHealth(*healthcheck)
	}
	value, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("load API configuration", "error", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, value); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("API service stopped", "error", err)
		return 1
	}
	return 0
}

func probeHealth(endpoint string) int {
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(endpoint)
	if err != nil {
		return 1
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func loadConfig(path string) (config, error) {
	value, err := decodeConfig(path)
	if err != nil {
		return config{}, err
	}
	if value.Listen == "" {
		value.Listen = "127.0.0.1:8080"
	}
	if err := validateListen(value.Listen); err != nil {
		return config{}, err
	}
	if err := validateConfig(value); err != nil {
		return config{}, err
	}
	return value, nil
}

func decodeConfig(path string) (config, error) {
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
	return value, nil
}

func validateListen(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return errors.New("listen must include host and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || (!ip.IsLoopback() && !ip.IsUnspecified()) {
		return errors.New("API listen address must be loopback or container-wildcard")
	}
	return nil
}

func validateConfig(value config) error {
	if value.DatabaseURL == "" || !filepath.IsAbs(value.ArtifactRoot) ||
		filepath.Clean(value.ArtifactRoot) != value.ArtifactRoot ||
		!filepath.IsAbs(value.RepositoryRoot) ||
		filepath.Clean(value.RepositoryRoot) != value.RepositoryRoot {
		return errors.New("incomplete API configuration")
	}
	if err := value.Policy.Validate(); err != nil || value.Policy.Repository.Root != value.RepositoryRoot {
		return errors.New("invalid or mismatched beta policy")
	}
	if err := controlapi.ValidatePrincipals(value.Principals); err != nil {
		return err
	}
	if value.TaskMaxAttempts != 0 &&
		(value.TaskMaxAttempts < controlapi.DefaultTaskMaxAttempts || value.TaskMaxAttempts > 10) {
		return errors.New("task_max_attempts must be between 2 and 10")
	}
	if value.Publication != nil && (value.Publication.RepositoryRoot != value.Policy.Repository.Root ||
		value.Publication.RepositoryOwner != value.Policy.Repository.Owner ||
		value.Publication.RepositoryName != value.Policy.Repository.Name ||
		value.Publication.Remote != value.Policy.Repository.Remote ||
		value.Publication.BaseBranch != value.Policy.Repository.BaseBranch) {
		return errors.New("publication configuration does not match beta policy")
	}
	return nil
}

func run( // skipcq: GO-R1005 -- explicit fail-closed startup composition
	ctx context.Context, value config,
) error {
	migrateCtx, cancelMigrate := context.WithTimeout(ctx, 30*time.Second)
	err := workflow.Migrate(migrateCtx, value.DatabaseURL)
	if err == nil {
		err = approval.Migrate(migrateCtx, value.DatabaseURL)
	}
	if err == nil {
		err = publication.Migrate(migrateCtx, value.DatabaseURL)
	}
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
	approvalLedger, err := approval.NewPostgresApprovalRepository(pool)
	if err != nil {
		return err
	}
	approvalService, err := approval.NewService(artifacts, workflow.NewStore(pool), approvalLedger)
	if err != nil {
		return err
	}
	publicationService, err := buildPublication(value.Publication, artifacts, pool)
	if err != nil {
		return err
	}
	workflowStore := workflow.NewStore(pool)
	runIntake, err := controlapi.NewRunIntakeCoordinator(
		pool, workflowStore, artifacts, value.RepositoryRoot, value.Policy,
	)
	if err != nil {
		return err
	}
	handler, err := controlapi.New(controlapi.Config{
		Principals: value.Principals, ServiceActorID: value.ServiceActorID,
		MaxBodyBytes:    value.MaxBodyBytes,
		TaskMaxAttempts: value.TaskMaxAttempts,
		TrustedChecks:   value.TrustedChecks,
		Policy:          value.Policy,
		Ready: func(readyCtx context.Context) error {
			if err := pool.Ping(readyCtx); err != nil {
				return err
			}
			if _, err := migration.Verify(readyCtx, value.DatabaseURL,
				workflow.MigrationSource(), registry.MigrationSource(), gateway.MigrationSource(),
				execution.MigrationSource(), verification.MigrationSource(),
				approval.MigrationSource(), publication.MigrationSource()); err != nil {
				return err
			}
			if _, err := os.Stat(value.ArtifactRoot); err != nil {
				return err
			}
			_, err := os.Stat(value.RepositoryRoot)
			return err
		},
	}, workflowStore, runtime.NewLedger(pool), runIntake, artifacts,
		runtime.NewBindingRepository(pool), approvalService, publicationService, slog.Default())
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: value.Listen, Handler: handler.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute,
	}
	errs := make(chan error, 1)
	go func() {
		slog.Info("API service ready", "listen", value.Listen)
		errs <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func buildPublication(
	value *publicationConfig, artifacts *artifact.Store, pool *pgxpool.Pool,
) (*publication.Service, error) {
	if value == nil {
		return nil, nil
	}
	var gitPublisher *publication.GitCommandPublisher
	var err error
	if value.GitPushCredentialFile == "" {
		gitPublisher, err = publication.NewGitCommandPublisher(
			value.RepositoryRoot, value.Remote, value.BaseBranch,
		)
	} else {
		gitPublisher, err = publication.NewAuthenticatedGitCommandPublisher(
			value.RepositoryRoot, value.Remote, value.BaseBranch, value.GitPushCredentialFile,
		)
	}
	if err != nil {
		return nil, err
	}
	credential, err := publication.NewFileCredential(value.TokenFile)
	if err != nil {
		return nil, err
	}
	pulls, err := publication.NewGitHubRESTClient(
		value.APIEndpoint, value.APIVersion, publication.DefaultMaxBodyBytes,
		publication.DefaultTimeout, credential,
	)
	if err != nil {
		return nil, err
	}
	ledger, err := publication.NewPostgresPublicationRepository(pool)
	if err != nil {
		return nil, err
	}
	return publication.NewService(publication.Config{
		RepositoryRoot: value.RepositoryRoot, RepositoryOwner: value.RepositoryOwner,
		RepositoryName: value.RepositoryName, Remote: value.Remote,
		BaseBranch: value.BaseBranch, BranchPrefix: value.BranchPrefix,
		ActorID: value.ActorID, APIEndpoint: value.APIEndpoint,
		APIVersion: value.APIVersion,
	}, artifact.Publication{Store: artifacts}, workflow.NewStore(pool),
		gitPublisher, pulls, ledger)
}
