package workflow

import (
	"context"
	"embed"

	"github.com/Standard-Syntax/basic/go/internal/migration"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func MigrationSource() migration.Source {
	return migration.Source{Files: migrationFiles, Directory: "migrations"}
}

func Migrate(ctx context.Context, connectionString string) error {
	return migration.Apply(ctx, connectionString, migrationFiles, "migrations")
}
