package database

import (
	"strings"
	"testing"
)

const dropRiverUp = "migrations/20261016000332_drop_river.up.sql"

// TestDropRiverRefusesLiveJobs checks the River drop only runs once River has
// drained: it fails while river_job holds a job still due to run, and drops
// River's tables, functions and type once none is left.
func TestDropRiverRefusesLiveJobs(t *testing.T) {
	ctx := t.Context()
	db, m := throwawayDB(t, "drop_river")
	if _, err := m.Migrate(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	exec := func(query string) error {
		t.Helper()
		_, err := db.DB.ExecContext(ctx, query)
		return err
	}
	body, err := sqlMigrations.ReadFile(dropRiverUp)
	if err != nil {
		t.Fatal(err)
	}

	// A stand-in for River's schema: the objects the migration drops.
	for _, q := range []string{
		`CREATE TYPE river_job_state AS ENUM ('available', 'cancelled', 'completed', 'discarded', 'pending', 'retryable', 'running', 'scheduled')`,
		`CREATE TABLE river_job (id bigserial PRIMARY KEY, state river_job_state NOT NULL)`,
		`CREATE TABLE river_leader (name text PRIMARY KEY)`,
		`CREATE TABLE river_migration (version bigint PRIMARY KEY)`,
		`CREATE FUNCTION river_job_notify() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`,
		`INSERT INTO river_job (state) VALUES ('completed'), ('scheduled')`,
	} {
		if err := exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	if err := exec(string(body)); err == nil || !strings.Contains(err.Error(), "still holds live jobs") {
		t.Fatalf("drop with a scheduled job = %v, want it refused", err)
	}
	if err := exec(`UPDATE river_job SET state = 'completed'`); err != nil {
		t.Fatal(err)
	}
	if err := exec(string(body)); err != nil {
		t.Fatalf("drop after River drained: %v", err)
	}
	var left int
	if err := db.DB.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM pg_class WHERE relname LIKE 'river\_%')
		     + (SELECT count(*) FROM pg_type WHERE typname = 'river_job_state')
		     + (SELECT count(*) FROM pg_proc WHERE proname LIKE 'river\_%')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d River objects left after the drop", left)
	}
	// On a database that never had River it is a no-op.
	if err := exec(string(body)); err != nil {
		t.Errorf("drop without River: %v", err)
	}
}
