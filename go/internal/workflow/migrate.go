package workflow

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func Migrate(ctx context.Context, connectionString string) error {
	connection, err := pgx.Connect(ctx, connectionString)
	if err != nil {
		return fmt.Errorf("connect for migrations: %w", err)
	}
	defer func() { _ = connection.Close(ctx) }()

	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock(719043625421948938)`); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() {
		_, _ = connection.Exec(context.Background(), `SELECT pg_advisory_unlock(719043625421948938)`)
	}()

	if _, err := connection.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version bigint PRIMARY KEY,
		name text NOT NULL,
		digest text NOT NULL CHECK (digest ~ '^[a-f0-9]{64}$'),
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	migrations, err := embeddedMigrations()
	if err != nil {
		return err
	}
	for _, migration := range migrations {
		if err := applyMigration(ctx, connection, migration); err != nil {
			return err
		}
	}
	return nil
}

type migration struct {
	version int64
	name    string
	digest  string
	body    []byte
}

func embeddedMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		migration, err := readMigration(entry.Name())
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, migration)
	}
	return migrations, nil
}

func readMigration(name string) (migration, error) {
	versionText, _, ok := strings.Cut(name, "_")
	if !ok {
		return migration{}, fmt.Errorf("invalid migration filename %q", name)
	}
	version, err := strconv.ParseInt(versionText, 10, 64)
	if err != nil {
		return migration{}, fmt.Errorf("invalid migration version %q: %w", name, err)
	}
	body, err := migrationFiles.ReadFile(path.Join("migrations", name))
	if err != nil {
		return migration{}, fmt.Errorf("read migration %q: %w", name, err)
	}
	sum := sha256.Sum256(body)
	return migration{
		version: version, name: name, body: body,
		digest: hex.EncodeToString(sum[:]),
	}, nil
}

func applyMigration(ctx context.Context, connection *pgx.Conn, migration migration) error {
	var stored string
	err := connection.QueryRow(ctx,
		`SELECT digest FROM schema_migrations WHERE version=$1`, migration.version,
	).Scan(&stored)
	if err == nil {
		if stored != migration.digest {
			return fmt.Errorf("migration %d digest changed", migration.version)
		}
		return nil
	}
	if err != pgx.ErrNoRows {
		return fmt.Errorf("read migration %d: %w", migration.version, err)
	}
	tx, err := connection.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", migration.version, err)
	}
	if err := executeMigration(ctx, tx, migration); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %d: %w", migration.version, err)
	}
	return nil
}

func executeMigration(ctx context.Context, tx pgx.Tx, migration migration) error {
	if _, err := tx.Exec(ctx, string(migration.body)); err != nil {
		return fmt.Errorf("apply migration %d: %w", migration.version, err)
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations(version,name,digest) VALUES($1,$2,$3)`,
		migration.version, migration.name, migration.digest)
	if err != nil {
		return fmt.Errorf("record migration %d: %w", migration.version, err)
	}
	return nil
}
