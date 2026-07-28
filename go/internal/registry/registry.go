// Package registry stores immutable, canonical agent manifests.
package registry

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"regexp"
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

var (
	validName    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	validVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	validDigest  = regexp.MustCompile(`^[0-9a-f]{64}$`)
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

func (r *Registry) Get(ctx context.Context, name, version string) (Record, error) {
	if !validName.MatchString(name) || !validVersion.MatchString(version) {
		return Record{}, ErrInvalidArgument
	}
	var record Record
	var storedName, storedVersion string
	err := r.pool.QueryRow(ctx, `SELECT
			agent_name,agent_version,manifest_digest,canonical_manifest,registered_at
		FROM agent_registrations WHERE agent_name=$1 AND agent_version=$2`,
		name, version,
	).Scan(
		&storedName, &storedVersion, &record.Digest,
		&record.CanonicalJSON, &record.RegisteredAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("get registration: %w", err)
	}
	return validateRecord(record, storedName, storedVersion)
}

func (r *Registry) GetByDigest(ctx context.Context, digest string) (Record, error) {
	if !validDigest.MatchString(digest) {
		return Record{}, ErrInvalidArgument
	}
	var record Record
	var storedName, storedVersion string
	err := r.pool.QueryRow(ctx, `SELECT
			agent_name,agent_version,manifest_digest,canonical_manifest,registered_at
		FROM agent_registrations WHERE manifest_digest=$1`,
		digest,
	).Scan(
		&storedName, &storedVersion, &record.Digest,
		&record.CanonicalJSON, &record.RegisteredAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("get registration by digest: %w", err)
	}
	return validateRecord(record, storedName, storedVersion)
}

func validateRecord(record Record, storedName, storedVersion string) (Record, error) {
	value, canonical, digest, err := manifest.Read(record.CanonicalJSON)
	if err != nil ||
		!bytes.Equal(canonical, record.CanonicalJSON) ||
		digest != record.Digest ||
		value.Agent.Name != storedName ||
		value.Agent.Version != storedVersion {
		return Record{}, ErrCorruptData
	}
	record.Manifest = value
	record.CanonicalJSON = append([]byte(nil), record.CanonicalJSON...)
	return record, nil
}
