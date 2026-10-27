package queues_test

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/pgtest"
)

// TestRetireRiverMovesLiveJobs checks the jobs River left unfinished reach the
// outbox, scheduled as they were, and River's schema goes in the same step.
func TestRetireRiverMovesLiveJobs(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

	// A stand-in for River's schema: the objects RetireRiver drops.
	for _, q := range []string{
		`CREATE TYPE river_job_state AS ENUM ('available', 'cancelled', 'completed', 'discarded', 'pending', 'retryable', 'running', 'scheduled')`,
		`CREATE TABLE river_job (id bigserial PRIMARY KEY, state river_job_state NOT NULL, kind text NOT NULL, args jsonb NOT NULL, scheduled_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE river_leader (name text PRIMARY KEY)`,
		`CREATE TABLE river_migration (version bigint PRIMARY KEY)`,
		`CREATE FUNCTION river_job_notify() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`,
	} {
		if _, err := db.DB.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	for _, j := range []struct {
		state, kind, args string
		at                time.Time
	}{
		{"scheduled", "submit_post_to_zernio", `{"post_id":"p-later"}`, at},
		{"running", "send_email", `{"user_id":"u1","template_key":"welcome","email_kind":"transactional","idempotency_key":"welcome:u1"}`, time.Now()},
		{"completed", "submit_post_to_zernio", `{"post_id":"p-done"}`, time.Now()},
		{"available", "cleanup_post_logs", `{}`, time.Now()},
		{"retryable", "build_weekly_report", `{"tenant_id":"t1","week_start":"2026-10-26"}`, time.Now()},
		{"scheduled", "billing_apply_scheduled_change", `{"change_id":"ch1"}`, at},
	} {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO river_job (state, kind, args, scheduled_at) VALUES ($1, $2, $3, $4)`,
			j.state, j.kind, j.args, j.at); err != nil {
			t.Fatal(err)
		}
	}

	moved, err := queues.RetireRiver(ctx, db.DB)
	if err != nil || moved != 4 {
		t.Fatalf("RetireRiver = %d, %v; want 4 moved", moved, err)
	}
	var scheduled time.Time
	if err := db.DB.QueryRowContext(ctx, `SELECT (payload->>'scheduledAt')::timestamptz FROM job_outbox
		WHERE kind = 'submit_post_to_zernio' AND payload->'submitPostToZernio'->>'postId' = 'p-later'`).Scan(&scheduled); err != nil {
		t.Fatalf("moved submit: %v", err)
	}
	if !scheduled.Equal(at) {
		t.Errorf("moved submit scheduled at %v, want %v", scheduled, at)
	}
	if n := count(t, db, `SELECT count(*) FROM job_outbox WHERE kind = 'send_email' AND payload->'sendEmail'->>'idempotencyKey' = 'welcome:u1'`); n != 1 {
		t.Errorf("moved send_email rows = %d, want 1", n)
	}
	if n := count(t, db, `SELECT count(*) FROM job_outbox WHERE kind = 'build_weekly_report' AND payload->'buildWeeklyReport'->>'weekStart' = '2026-10-26'`); n != 1 {
		t.Errorf("moved build_weekly_report rows = %d, want 1", n)
	}
	if err := db.DB.QueryRowContext(ctx, `SELECT (payload->>'scheduledAt')::timestamptz FROM job_outbox
		WHERE kind = 'billing_apply_scheduled_change' AND payload->'billingApplyScheduledChange'->>'changeId' = 'ch1'`).Scan(&scheduled); err != nil || !scheduled.Equal(at) {
		t.Errorf("moved scheduled change: %v at %v, want %v", err, scheduled, at)
	}
	if n := count(t, db, `SELECT count(*) FROM job_outbox WHERE payload->'submitPostToZernio'->>'postId' = 'p-done' OR kind = 'cleanup_post_logs'`); n != 0 {
		t.Errorf("finished or periodic jobs moved: %d rows", n)
	}
	if n := count(t, db, `
		SELECT (SELECT count(*) FROM pg_class WHERE relname LIKE 'river\_%')
		     + (SELECT count(*) FROM pg_type WHERE typname = 'river_job_state')
		     + (SELECT count(*) FROM pg_proc WHERE proname LIKE 'river\_%')`); n != 0 {
		t.Errorf("%d River objects left", n)
	}

	// Without River it does nothing.
	if moved, err := queues.RetireRiver(ctx, db.DB); err != nil || moved != 0 {
		t.Errorf("second RetireRiver = %d, %v; want 0, nil", moved, err)
	}
}
