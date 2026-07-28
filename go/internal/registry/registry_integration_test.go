//go:build integration

package registry

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func integrationRegistry(t *testing.T) (*Registry, *pgxpool.Pool, string) {
	t.Helper()
	connectionString := os.Getenv("TEST_DATABASE_URL")
	if connectionString == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if err := Migrate(t.Context(), connectionString); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(t.Context(), connectionString)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return New(pool), pool, connectionString
}

func uniqueManifest(t *testing.T, description string) []byte {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(fixture(t), &value); err != nil {
		t.Fatal(err)
	}
	agent := value["agent"].(map[string]any)
	agent["name"] = "registry-" + uuid.NewString()[:12]
	value["metadata"].(map[string]any)["description"] = description
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func changedManifest(t *testing.T, raw []byte, description string) []byte {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["metadata"].(map[string]any)["description"] = description
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRegisterReplayLookupsAndImmutability(t *testing.T) {
	registry, pool, _ := integrationRegistry(t)
	raw := uniqueManifest(t, "registered")
	first, err := registry.Register(t.Context(), raw)
	if err != nil || !first.Created {
		t.Fatalf("first register = %+v, %v", first, err)
	}
	raw[0] = ' '
	replay, err := registry.Register(t.Context(), first.CanonicalJSON)
	if err != nil || replay.Created {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	if replay.Digest != first.Digest || !replay.RegisteredAt.Equal(first.RegisteredAt) {
		t.Fatal("replay did not return original record")
	}

	byID, err := registry.Get(
		t.Context(), first.Manifest.Agent.Name, first.Manifest.Agent.Version,
	)
	if err != nil || string(byID.CanonicalJSON) != string(first.CanonicalJSON) {
		t.Fatalf("identity lookup = %+v, %v", byID, err)
	}
	byDigest, err := registry.GetByDigest(t.Context(), first.Digest)
	if err != nil || string(byDigest.CanonicalJSON) != string(first.CanonicalJSON) {
		t.Fatalf("digest lookup = %+v, %v", byDigest, err)
	}

	if _, err := pool.Exec(t.Context(), `UPDATE agent_registrations
		SET canonical_manifest=canonical_manifest WHERE agent_name=$1`,
		first.Manifest.Agent.Name); err == nil {
		t.Fatal("database allowed registration update")
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM agent_registrations
		WHERE agent_name=$1`, first.Manifest.Agent.Name); err == nil {
		t.Fatal("database allowed registration delete")
	}
}

func TestConflictMissingInvalidRollbackAndCorruption(t *testing.T) {
	registry, pool, _ := integrationRegistry(t)
	raw := uniqueManifest(t, "winner")
	winner, err := registry.Register(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register(
		t.Context(), changedManifest(t, raw, "replacement"),
	); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if _, err := registry.Get(t.Context(), "absent-agent", "1.0.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing identity error = %v", err)
	}
	missingDigest := "0" + winner.Digest[1:]
	if winner.Digest[0] == '0' {
		missingDigest = "1" + winner.Digest[1:]
	}
	if _, err := registry.GetByDigest(t.Context(), missingDigest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing digest error = %v", err)
	}

	var before, after int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_registrations`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register(t.Context(), []byte(`{"agent":`)); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("invalid manifest error = %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_registrations`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("failed registration wrote rows: before=%d after=%d", before, after)
	}

	name := "corrupt-" + uuid.NewString()[:12]
	digest := missingDigest
	if _, err := pool.Exec(t.Context(), `INSERT INTO agent_registrations
		(agent_name,agent_version,manifest_digest,canonical_manifest)
		VALUES($1,'1.0.0',$2,'{}')`, name, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Get(t.Context(), name, "1.0.0"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("corrupt lookup error = %v", err)
	}
}

func TestConcurrentRegistrationConverges(t *testing.T) {
	registry, _, _ := integrationRegistry(t)
	raw := uniqueManifest(t, "same")
	const calls = 12
	var created atomic.Int32
	errs := make(chan error, calls)
	var group sync.WaitGroup
	for range calls {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := registry.Register(context.Background(), raw)
			if result.Created {
				created.Add(1)
			}
			errs <- err
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("identical concurrent register: %v", err)
		}
	}
	if created.Load() != 1 {
		t.Fatalf("created count = %d", created.Load())
	}

	raw = uniqueManifest(t, "candidate")
	variants := make([][]byte, calls)
	for index := range calls {
		variants[index] = changedManifest(t, raw, fmt.Sprintf("candidate-%d", index))
	}
	var conflicts atomic.Int32
	created.Store(0)
	errs = make(chan error, calls)
	for index := range calls {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := registry.Register(context.Background(), variants[index])
			if result.Created {
				created.Add(1)
			}
			if errors.Is(err, ErrVersionConflict) {
				conflicts.Add(1)
				err = nil
			}
			errs <- err
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("conflicting concurrent register: %v", err)
		}
	}
	if created.Load() != 1 || conflicts.Load() != calls-1 {
		t.Fatalf("created=%d conflicts=%d", created.Load(), conflicts.Load())
	}
}

func TestMigrationReplayAndDigestProtection(t *testing.T) {
	_, pool, connectionString := integrationRegistry(t)
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- Migrate(context.Background(), connectionString) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("migration replay: %v", err)
		}
	}
	var original string
	if err := pool.QueryRow(t.Context(),
		`SELECT digest FROM schema_migrations WHERE version=6`,
	).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(),
		`UPDATE schema_migrations SET digest=repeat('0',64) WHERE version=6`); err != nil {
		t.Fatal(err)
	}
	migrateErr := Migrate(t.Context(), connectionString)
	if _, err := pool.Exec(t.Context(),
		`UPDATE schema_migrations SET digest=$1 WHERE version=6`, original); err != nil {
		t.Fatal(err)
	}
	if migrateErr == nil {
		t.Fatal("changed migration digest was accepted")
	}
	data, err := migrationFiles.ReadFile("migrations/0006_agent_registry.sql")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if original != fmt.Sprintf("%x", sum) {
		t.Fatal("ledger digest did not match embedded migration")
	}
}
