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

	"github.com/Standard-Syntax/basic/go/internal/artifact"
	"github.com/Standard-Syntax/basic/go/internal/controlapi"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/jackc/pgx/v5/pgxpool"
)

type config struct {
	Listen           string                 `json:"listen"`
	DatabaseURL      string                 `json:"database_url"`
	ArtifactRoot     string                 `json:"artifact_root"`
	ServiceActorID   string                 `json:"service_actor_id"`
	MaxArtifactBytes int64                  `json:"max_artifact_bytes"`
	MaxBodyBytes     int64                  `json:"max_body_bytes"`
	TrustedChecks    []string               `json:"trusted_checks"`
	Principals       []controlapi.Principal `json:"principals"`
}

func main() {
	os.Exit(mainExit())
}

func mainExit() int {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	configPath := flag.String("config", "", "absolute path to strict JSON configuration")
	flag.Parse()
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
	if value.Listen == "" {
		value.Listen = "127.0.0.1:8080"
	}
	host, _, err := net.SplitHostPort(value.Listen)
	if err != nil {
		return config{}, errors.New("listen must include host and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return config{}, errors.New("API listen address must be loopback")
	}
	if value.DatabaseURL == "" || !filepath.IsAbs(value.ArtifactRoot) ||
		filepath.Clean(value.ArtifactRoot) != value.ArtifactRoot {
		return config{}, errors.New("incomplete API configuration")
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
	handler, err := controlapi.New(controlapi.Config{
		Principals: value.Principals, ServiceActorID: value.ServiceActorID,
		MaxBodyBytes:  value.MaxBodyBytes,
		TrustedChecks: value.TrustedChecks,
	}, workflow.NewStore(pool), runtime.NewLedger(pool), artifacts,
		runtime.NewBindingRepository(pool), slog.Default())
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
