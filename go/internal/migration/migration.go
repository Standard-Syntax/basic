// Package migration applies embedded, forward-only PostgreSQL migrations.
package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

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
