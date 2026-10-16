package queues_test

import (
	"context"
	"errors"
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

// TestEnqueuerWritesTheOutbox checks every enqueue lands in job_outbox inside
// the caller's transaction: it commits with the change and vanishes with a
// rollback.
func TestEnqueuerWritesTheOutbox(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	enq := queues.NewEnqueuer(outbox.NewWriter(db.DB))

	if err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := enq.EnqueueWelcomeEmailTx(ctx, tx.Tx, "u-out", "t-out"); err != nil {
			return err
		}
		return enq.EnqueueSubmitTx(ctx, tx.Tx, "p-out")
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM job_outbox WHERE kind = 'send_email' AND payload->'sendEmail'->>'userId' = 'u-out'`); n != 1 {
		t.Errorf("outbox send_email rows = %d, want 1", n)
	}
	if n := count(t, db, `SELECT count(*) FROM job_outbox WHERE kind = 'submit_post_to_zernio' AND payload->'submitPostToZernio'->>'postId' = 'p-out'`); n != 1 {
		t.Errorf("outbox submit rows = %d, want 1", n)
	}

	errRollback := errors.New("rollback")
	if err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := enq.EnqueueSubmitTx(ctx, tx.Tx, "p-rolled-back"); err != nil {
			return err
		}
		return errRollback
	}); !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM job_outbox WHERE payload->'submitPostToZernio'->>'postId' = 'p-rolled-back'`); n != 0 {
		t.Errorf("a rolled-back enqueue left %d rows", n)
	}

	// The drip is three scheduled sends.
	if err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return enq.EnqueueDripTx(ctx, tx.Tx, "u-drip", "t-out")
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM job_outbox WHERE payload->'sendEmail'->>'userId' = 'u-drip' AND payload ? 'scheduledAt'`); n != 3 {
		t.Errorf("scheduled drip rows = %d, want 3", n)
	}

	var nilEnq *queues.Enqueuer
	if err := nilEnq.EnqueueCancel(ctx, "p1", queues.CancelTargetDraft, "u1"); err == nil {
		t.Error("a cancel with no enqueuer wired succeeded")
	}
}
