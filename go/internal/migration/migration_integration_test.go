//go:build integration

package migration

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var verificationSource = Source{
	Files: fstest.MapFS{
		"migrations/0001_alpha.sql": {Data: []byte("SELECT 1;\n")},
	},
	Directory: "migrations",
}

func TestVerifyExistingMigrationLedger(t *testing.T) {
	tests := []struct {
		name      string
		apply     bool
		mutate    string
		arguments []any
		wantError string
	}{
		{name: "success", apply: true},
		{name: "missing ledger", wantError: "migration ledger missing"},
		{name: "pending", apply: true, mutate: `DELETE FROM schema_migrations WHERE version=1`, wantError: "migration 1 pending"},
		{name: "renamed", apply: true, mutate: `UPDATE schema_migrations SET name='0001_renamed.sql' WHERE version=1`, wantError: "migration 1 name changed"},
		{name: "digest drift", apply: true, mutate: `UPDATE schema_migrations SET digest=$1 WHERE version=1`, arguments: []any{strings.Repeat("0", 64)}, wantError: "migration 1 digest changed"},
		{name: "unexpected", apply: true, mutate: `INSERT INTO schema_migrations(version,name,digest) VALUES(2,'0002_unexpected.sql',$1)`, arguments: []any{strings.Repeat("1", 64)}, wantError: "unexpected migration 2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connectionString := isolatedDatabase(t)
			if test.apply {
				if err := Apply(context.Background(), connectionString, verificationSource.Files, verificationSource.Directory); err != nil {
					t.Fatal(err)
				}
			}
			if test.mutate != "" {
				mutateLedger(t, connectionString, test.mutate, test.arguments...)
			}
			expected, err := Verify(context.Background(), connectionString, verificationSource)
			if test.wantError == "" {
				if err != nil || len(expected) != 1 {
					t.Fatalf("Verify() = %#v, %v", expected, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Verify() error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func isolatedDatabase(t *testing.T) string {
	t.Helper()
	connectionString := os.Getenv("TEST_DATABASE_URL")
	if connectionString == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, connectionString)
	if err != nil {
		t.Fatal(err)
	}
	schema := "migration_verify_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := connection.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		connection.Close(ctx)
		t.Fatal(err)
	}
	if err := connection.Close(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cleanupErr := pgx.Connect(context.Background(), connectionString)
		if cleanupErr != nil {
			t.Errorf("connect for cleanup: %v", cleanupErr)
			return
		}
		defer cleanup.Close(context.Background())
		if _, cleanupErr = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); cleanupErr != nil {
			t.Errorf("drop schema: %v", cleanupErr)
		}
	})
	parsed, err := url.Parse(connectionString)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func mutateLedger(t *testing.T, connectionString, statement string, arguments ...any) {
	t.Helper()
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, connectionString)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(ctx)
	if _, err := connection.Exec(ctx, statement, arguments...); err != nil {
		t.Fatal(err)
	}
}
