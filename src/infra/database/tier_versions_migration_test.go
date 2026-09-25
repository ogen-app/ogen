package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

// TestTierVersionsMigrationRollback verifies the CON-243 migration applies and
// rolls back cleanly (a PRD acceptance criterion): migrate up, roll the group
// back (running every down migration, newest — CON-243 — first), then re-apply.
func TestTierVersionsMigrationRollback(t *testing.T) {
	ctx := t.Context()
	db, m := throwawayDB(t, "rbtest")

	if _, err := m.Migrate(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if !tableExists(ctx, t, db, "tenant_tier_versions") {
		t.Fatal("tenant_tier_versions missing after migrate up")
	}

	if _, err := m.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if tableExists(ctx, t, db, "tenant_tier_versions") {
		t.Fatal("tenant_tier_versions still present after rollback")
	}

	if _, err := m.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate up: %v", err)
	}
	if !tableExists(ctx, t, db, "tenant_tier_versions") {
		t.Fatal("tenant_tier_versions missing after re-apply")
	}
}

// throwawayDB creates an empty database named after name and returns it with an
// initialised (not yet run) migrator; both are dropped when the test ends. It
// uses TEST_DATABASE_DSN directly rather than pgtest, which would create an
// import cycle for this internal test package.
func throwawayDB(t *testing.T, name string) (*bun.DB, *migrate.Migrator) {
	t.Helper()
	adminDSN := os.Getenv("TEST_DATABASE_DSN")
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_DSN not set")
	}
	ctx := t.Context()

	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	dbName := fmt.Sprintf("ogen_%s_%d", name, os.Getpid())
	_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbName)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName) })

	dsn, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse admin DSN: %v", err)
	}
	dsn.Path = "/" + dbName
	db, err := New(dsn.String(), false)
	if err != nil {
		t.Fatalf("connect to throwaway db: %v", err)
	}
	// Registered after the DROP cleanup, so it runs first: the connection pool
	// must be closed before the database can be dropped.
	t.Cleanup(func() { _ = db.Close() })

	migrations := migrate.NewMigrations()
	if err := migrations.Discover(sqlMigrations); err != nil {
		t.Fatalf("discover migrations: %v", err)
	}
	m := migrate.NewMigrator(db, migrations, migrate.WithMarkAppliedOnSuccess(true))
	if err := m.Init(ctx); err != nil {
		t.Fatalf("init migrator: %v", err)
	}
	return db, m
}

func tableExists(ctx context.Context, t *testing.T, db *bun.DB, table string) bool {
	t.Helper()
	var reg *string
	if err := db.NewRaw("SELECT to_regclass(?)::text", table).Scan(ctx, &reg); err != nil {
		t.Fatalf("to_regclass(%s): %v", table, err)
	}
	return reg != nil
}
