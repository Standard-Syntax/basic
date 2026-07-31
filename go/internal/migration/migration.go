// Package migration applies embedded, forward-only PostgreSQL migrations.
package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type Source struct {
	Files     fs.FS
	Directory string
}

type Expected struct {
	Version      int64
	Name, Digest string
}

func Describe(sources ...Source) ([]Expected, error) {
	var result []Expected
	seen := map[int64]bool{}
	for _, source := range sources {
		items, err := readAll(source.Files, source.Directory)
		if err != nil {
			return nil, err
		}
		for _, value := range items {
			if seen[value.version] {
				return nil, fmt.Errorf("duplicate migration version %d", value.version)
			}
			seen[value.version] = true
			result = append(result, Expected{Version: value.version, Name: value.name, Digest: value.digest})
		}
	}
	slices.SortFunc(result, func(a, b Expected) int { return int(a.Version - b.Version) })
	return result, nil
}

// Verify checks the existing ledger without applying migrations or creating schema objects.
func Verify(ctx context.Context, connectionString string, sources ...Source) ([]Expected, error) {
	expected, err := Describe(sources...)
	if err != nil {
		return nil, err
	}
	connection, err := connectReadOnly(ctx, connectionString)
	if err != nil {
		return nil, err
	}
	defer func() { _ = connection.Close(ctx) }()
	actual, err := readLedger(ctx, connection)
	if err != nil {
		return nil, err
	}
	if err := compareMigrations(expected, actual); err != nil {
		return nil, err
	}
	return expected, nil
}

func connectReadOnly(ctx context.Context, connectionString string) (*pgx.Conn, error) {
	config, err := pgx.ParseConfig(connectionString)
	if err != nil {
		return nil, fmt.Errorf("parse migration connection: %w", err)
	}
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["default_transaction_read_only"] = "on"
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect for migration verification: %w", err)
	}
	return connection, nil
}

func readLedger(ctx context.Context, connection *pgx.Conn) (map[int64]Expected, error) {
	var ledger *string
	if err := connection.QueryRow(ctx, `SELECT to_regclass('schema_migrations')::text`).Scan(&ledger); err != nil || ledger == nil {
		return nil, errors.New("migration ledger missing")
	}
	rows, err := connection.Query(ctx, `SELECT version,name,digest FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read migration ledger: %w", err)
	}
	defer rows.Close()
	actual := map[int64]Expected{}
	for rows.Next() {
		var item Expected
		if err := rows.Scan(&item.Version, &item.Name, &item.Digest); err != nil {
			return nil, err
		}
		actual[item.Version] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return actual, nil
}

func compareMigrations(expected []Expected, actual map[int64]Expected) error {
	for _, item := range expected {
		stored, ok := actual[item.Version]
		if !ok {
			return fmt.Errorf("migration %d pending", item.Version)
		}
		if stored.Name != item.Name {
			return fmt.Errorf("migration %d name changed", item.Version)
		}
		if stored.Digest != item.Digest {
			return fmt.Errorf("migration %d digest changed", item.Version)
		}
	}
	expectedVersions := make(map[int64]bool, len(expected))
	for _, item := range expected {
		expectedVersions[item.Version] = true
	}
	for version := range actual {
		if !expectedVersions[version] {
			return fmt.Errorf("unexpected migration %d", version)
		}
	}
	return nil
}

const advisoryLock int64 = 719043625421948938

// Apply serializes and applies the SQL files in directory, recording their
// content digests in the shared schema_migrations ledger.
func Apply(ctx context.Context, connectionString string, files fs.FS, directory string) error {
	connection, err := pgx.Connect(ctx, connectionString)
	if err != nil {
		return fmt.Errorf("connect for migrations: %w", err)
	}
	defer func() { _ = connection.Close(ctx) }()

	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLock); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() {
		_, _ = connection.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLock)
	}()

	if _, err := connection.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version bigint PRIMARY KEY,
		name text NOT NULL,
		digest text NOT NULL CHECK (digest ~ '^[a-f0-9]{64}$'),
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	migrations, err := readAll(files, directory)
	if err != nil {
		return err
	}
	for _, item := range migrations {
		if err := applyOne(ctx, connection, item); err != nil {
			return err
		}
	}
	return nil
}

type item struct {
	version int64
	name    string
	digest  string
	body    []byte
}

func readAll(files fs.FS, directory string) ([]item, error) {
	entries, err := fs.ReadDir(files, directory)
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	result := make([]item, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		value, err := read(files, directory, entry.Name())
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].version < result[j].version })
	for index := 1; index < len(result); index++ {
		if result[index-1].version == result[index].version {
			return nil, fmt.Errorf(
				"duplicate migration version %d in %q and %q",
				result[index].version, result[index-1].name, result[index].name,
			)
		}
	}
	return result, nil
}

func read(files fs.FS, directory, name string) (item, error) {
	versionText, _, ok := strings.Cut(name, "_")
	if !ok {
		return item{}, fmt.Errorf("invalid migration filename %q", name)
	}
	version, err := strconv.ParseInt(versionText, 10, 64)
	if err != nil {
		return item{}, fmt.Errorf("invalid migration version %q: %w", name, err)
	}
	body, err := fs.ReadFile(files, path.Join(directory, name))
	if err != nil {
		return item{}, fmt.Errorf("read migration %q: %w", name, err)
	}
	sum := sha256.Sum256(body)
	return item{
		version: version, name: name, body: body,
		digest: hex.EncodeToString(sum[:]),
	}, nil
}

func applyOne(ctx context.Context, connection *pgx.Conn, value item) error {
	var stored string
	err := connection.QueryRow(ctx,
		`SELECT digest FROM schema_migrations WHERE version=$1`, value.version,
	).Scan(&stored)
	if err == nil {
		if stored != value.digest {
			return fmt.Errorf("migration %d digest changed", value.version)
		}
		return nil
	}
	if err != pgx.ErrNoRows {
		return fmt.Errorf("read migration %d: %w", value.version, err)
	}
	tx, err := connection.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", value.version, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, string(value.body)); err != nil {
		return fmt.Errorf("apply migration %d: %w", value.version, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations(version,name,digest) VALUES($1,$2,$3)`,
		value.version, value.name, value.digest); err != nil {
		return fmt.Errorf("record migration %d: %w", value.version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %d: %w", value.version, err)
	}
	return nil
}
