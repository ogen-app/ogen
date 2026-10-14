package queues_test

import (
	"context"
	"testing"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/jobs/outbox"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/pgtest"
)

func count(t *testing.T, db *bun.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.DB.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestEnqueuerRoutesRelayedKindsToTheOutbox checks the per-kind cutover: a
// relayed kind is written to the outbox in the caller's transaction and never
// reaches ogen's River queue, while every other kind still does.
func TestEnqueuerRoutesRelayedKindsToTheOutbox(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	enq, err := queues.NewInsertOnlyEnqueuer(db)
	if err != nil {
		t.Fatal(err)
	}
	enq.Relay(outbox.NewWriter(db.DB), []string{"send_email"})

	if err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := enq.EnqueueWelcomeEmailTx(ctx, tx.Tx, "u-relay", "t-relay"); err != nil {
			return err
		}
		return enq.EnqueueSubmitTx(ctx, tx.Tx, "p-relay")
	}); err != nil {
		t.Fatal(err)
	}

	if n := count(t, db, `SELECT count(*) FROM job_outbox WHERE kind = 'send_email' AND payload->'sendEmail'->>'userId' = 'u-relay'`); n != 1 {
		t.Errorf("outbox send_email rows = %d, want 1", n)
	}
	if n := count(t, db, `SELECT count(*) FROM river_job WHERE kind = 'send_email' AND args->>'user_id' = 'u-relay'`); n != 0 {
		t.Errorf("river_job send_email rows = %d, want 0", n)
	}
	if n := count(t, db, `SELECT count(*) FROM river_job WHERE kind = 'submit_post_to_zernio' AND args->>'post_id' = 'p-relay'`); n != 1 {
		t.Errorf("river_job submit rows = %d, want 1", n)
	}
	if n := count(t, db, `SELECT count(*) FROM job_outbox WHERE kind = 'submit_post_to_zernio'`); n != 0 {
		t.Errorf("outbox submit rows = %d, want 0", n)
	}

	// The drip is three scheduled sends, all relayed.
	if err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return enq.EnqueueDripTx(ctx, tx.Tx, "u-drip", "t-relay")
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM job_outbox WHERE payload->'sendEmail'->>'userId' = 'u-drip' AND payload ? 'scheduledAt'`); n != 3 {
		t.Errorf("scheduled drip rows = %d, want 3", n)
	}
}
