package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

// TestTierVersionsMigrationRollback verifies the CON-243 migration applies and
// rolls back cleanly (a PRD acceptance criterion): migrate up, roll the group
// back (running every down migration, newest — CON-243 — first), then re-apply.
// Uses TEST_DATABASE_DSN directly on a throwaway database, so it does not depend
// on pgtest (which would create an import cycle for this internal test package).
func TestTierVersionsMigrationRollback(t *testing.T) {
	adminDSN := os.Getenv("TEST_DATABASE_DSN")
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_DSN not set")
	}
	ctx := t.Context()

	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()

	name := fmt.Sprintf("ogen_rbtest_%d", os.Getpid())
	_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	defer admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name)

	db, err := New(strings.Replace(adminDSN, "/postgres?", "/"+name+"?", 1), false)
	if err != nil {
		t.Fatalf("connect to throwaway db: %v", err)
	}
	defer db.Close()

	migrations := migrate.NewMigrations()
	if err := migrations.Discover(sqlMigrations); err != nil {
		t.Fatalf("discover migrations: %v", err)
	}
	m := migrate.NewMigrator(db, migrations, migrate.WithMarkAppliedOnSuccess(true))
	if err := m.Init(ctx); err != nil {
		t.Fatalf("init migrator: %v", err)
	}

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

func tableExists(ctx context.Context, t *testing.T, db *bun.DB, table string) bool {
	t.Helper()
	var reg *string
	if err := db.NewRaw("SELECT to_regclass(?)::text", table).Scan(ctx, &reg); err != nil {
		t.Fatalf("to_regclass(%s): %v", table, err)
	}
	return reg != nil
}
