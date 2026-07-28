// Package registry stores immutable, canonical agent manifests.
package registry

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/manifest"
	"github.com/Standard-Syntax/basic/go/internal/migration"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidArgument = errors.New("invalid registry argument")
	ErrInvalidManifest = errors.New("invalid agent manifest")
	ErrVersionConflict = errors.New("agent version conflict")
	ErrNotFound        = errors.New("agent registration not found")
	ErrCorruptData     = errors.New("corrupt persisted agent registration")
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Registry struct {
	pool *pgxpool.Pool
}

type Record struct {
	Manifest      manifest.Manifest
	CanonicalJSON []byte
	Digest        string
	RegisteredAt  time.Time
}

type RegisterResult struct {
	Record
	Created bool
}

func New(pool *pgxpool.Pool) *Registry {
	return &Registry{pool: pool}
}

func Migrate(ctx context.Context, connectionString string) error {
	return migration.Apply(ctx, connectionString, migrationFiles, "migrations")
}

func (r *Registry) Register(ctx context.Context, rawManifest []byte) (RegisterResult, error) {
	value, canonical, digest, err := manifest.Read(rawManifest)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("begin registration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(
		hashtextextended($1 || ':' || $2, 719043625421948938))`,
		value.Agent.Name, value.Agent.Version); err != nil {
		return RegisterResult{}, fmt.Errorf("lock agent version: %w", err)
	}

	var (
		storedDigest    string
		storedCanonical []byte
		registeredAt    time.Time
	)
	err = tx.QueryRow(ctx, `SELECT manifest_digest,canonical_manifest,registered_at
		FROM agent_registrations WHERE agent_name=$1 AND agent_version=$2`,
		value.Agent.Name, value.Agent.Version,
	).Scan(&storedDigest, &storedCanonical, &registeredAt)
	if err == nil {
		if storedDigest != digest {
			return RegisterResult{}, ErrVersionConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return RegisterResult{}, fmt.Errorf("commit registration replay: %w", err)
		}
		return RegisterResult{
			Record: Record{
				Manifest: value, CanonicalJSON: append([]byte(nil), storedCanonical...),
				Digest: storedDigest, RegisteredAt: registeredAt,
			},
			Created: false,
		}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RegisterResult{}, fmt.Errorf("read agent version: %w", err)
	}

	err = tx.QueryRow(ctx, `INSERT INTO agent_registrations
			(agent_name,agent_version,manifest_digest,canonical_manifest)
		VALUES($1,$2,$3,$4) RETURNING registered_at`,
		value.Agent.Name, value.Agent.Version, digest, canonical,
	).Scan(&registeredAt)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("insert registration: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RegisterResult{}, fmt.Errorf("commit registration: %w", err)
	}
	return RegisterResult{
		Record: Record{
			Manifest: value, CanonicalJSON: append([]byte(nil), canonical...),
			Digest: digest, RegisteredAt: registeredAt,
		},
		Created: true,
	}, nil
}
