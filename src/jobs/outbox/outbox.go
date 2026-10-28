// Package outbox hands jobs to river-service. A job is written to the
// job_outbox table in the same transaction as the change that caused it, so it
// exists if and only if that change commits; the relay then delivers it to
// river-service's JobService and retries until it is accepted. A river-service
// outage only builds a backlog: no request ever waits on it.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/protobuf/encoding/protojson"

	jobsv1 "github.com/ogen-app/ogen/gen/jobs/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// notifyChannel is the Postgres channel a committed write signals, so the
// relay delivers within moments instead of at its next poll.
const notifyChannel = "job_outbox"

// execer is the part of *sql.Tx and *sql.DB a write needs.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Writer records jobs in the outbox.
type Writer struct {
	db *sql.DB
}

// NewWriter returns a writer over db.
func NewWriter(db *sql.DB) *Writer { return &Writer{db: db} }

// WriteTx records the job in the caller's transaction. kind is the job kind
// and req carries its args and schedule; the idempotency key and request id
// are filled in at delivery.
func (w *Writer) WriteTx(ctx context.Context, tx *sql.Tx, kind string, req *jobsv1.EnqueueRequest) error {
	_, err := write(ctx, tx, kind, "", req)
	return err
}

// Write records the job in a transaction of its own, for callers that have
// none.
func (w *Writer) Write(ctx context.Context, kind string, req *jobsv1.EnqueueRequest) error {
	_, err := w.WriteOnce(ctx, kind, "", req)
	return err
}

// WriteOnce is Write for a job that must be queued once per dedupeKey: while
// a row with the same key is kept, the write is dropped and reports false.
// An empty key always writes.
func (w *Writer) WriteOnce(ctx context.Context, kind, dedupeKey string, req *jobsv1.EnqueueRequest) (bool, error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("outbox: begin: %w", err)
	}
	written, err := write(ctx, tx, kind, dedupeKey, req)
	if err != nil {
		_ = tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("outbox: commit: %w", err)
	}
	return written, nil
}

func write(ctx context.Context, ex execer, kind, dedupeKey string, req *jobsv1.EnqueueRequest) (bool, error) {
	id, err := models.NewID()
	if err != nil {
		return false, fmt.Errorf("outbox: new id: %w", err)
	}
	payload, err := protojson.Marshal(req)
	if err != nil {
		return false, fmt.Errorf("outbox: encode %s: %w", kind, err)
	}
	carrier, err := traceCarrier(ctx)
	if err != nil {
		return false, err
	}
	requestID, _ := logging.RequestIDFrom(ctx)
	res, err := ex.ExecContext(ctx, `
		INSERT INTO job_outbox (id, kind, payload, trace_carrier, request_id, dedupe_key)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''))
		ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING`,
		id, kind, payload, carrier, requestID, dedupeKey)
	if err != nil {
		return false, fmt.Errorf("outbox: insert %s: %w", kind, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return false, nil
	}
	// Delivered on commit; dropped with a rolled-back transaction.
	if _, err := ex.ExecContext(ctx, `SELECT pg_notify($1, '')`, notifyChannel); err != nil {
		return false, fmt.Errorf("outbox: notify: %w", err)
	}
	return true, nil
}

// traceCarrier captures the trace context of ctx, so the job continues the
// trace of the request that enqueued it. Nil when ctx carries none.
func traceCarrier(ctx context.Context) ([]byte, error) {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(carrier)
	if err != nil {
		return nil, fmt.Errorf("outbox: encode trace context: %w", err)
	}
	return b, nil
}
