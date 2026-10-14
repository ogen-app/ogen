package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/encoding/protojson"

	jobsv1 "github.com/ogen-app/ogen/gen/jobs/v1"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// Relay health, exposed at /debug/vars.
var (
	relaySent    = expvar.NewInt("ogen_outbox_sent")
	relayFailed  = expvar.NewInt("ogen_outbox_failed")
	relayPending = expvar.NewInt("ogen_outbox_pending")
	// Age in seconds of the oldest undelivered job; rising means river-service
	// is unreachable or refusing a kind.
	relayLagSeconds = expvar.NewInt("ogen_outbox_lag_seconds")
)

const (
	// sendTimeout bounds one Enqueue call.
	sendTimeout = 5 * time.Second
	// maxBackoff caps the wait between retries of one row.
	maxBackoff = 5 * time.Minute
	// cleanupEvery is how often delivered rows past retention are deleted.
	cleanupEvery = time.Hour
	// relistenAfter is the wait before re-opening a dropped LISTEN connection.
	relistenAfter = 5 * time.Second
)

// Sender delivers one job to river-service. The jobs.v1 JobService client
// implements it.
type Sender interface {
	Enqueue(ctx context.Context, req *jobsv1.EnqueueRequest) (*jobsv1.EnqueueResponse, error)
}

// RelayConfig tunes the relay.
type RelayConfig struct {
	// DSN opens the dedicated connection the relay LISTENs on. Empty polls
	// only.
	DSN          string
	PollInterval time.Duration
	BatchSize    int
	Retention    time.Duration
}

// Relay delivers outbox rows to river-service. Several ogen replicas can run
// it at once: each claims rows with SKIP LOCKED, and river-service
// de-duplicates on the row id, so a row is never enqueued twice.
type Relay struct {
	db     *sql.DB
	sender Sender
	cfg    RelayConfig
	tracer trace.Tracer
}

// NewRelay returns a relay over db that delivers through sender.
func NewRelay(db *sql.DB, sender Sender, cfg RelayConfig) *Relay {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 7 * 24 * time.Hour
	}
	return &Relay{db: db, sender: sender, cfg: cfg, tracer: otel.Tracer("ogen/outbox")}
}

// Run delivers due rows until ctx ends: whenever a write signals, and at
// least every PollInterval.
func (r *Relay) Run(ctx context.Context) {
	wake := make(chan struct{}, 1)
	if r.cfg.DSN != "" {
		go r.listen(ctx, wake)
	}
	poll := time.NewTicker(r.cfg.PollInterval)
	defer poll.Stop()
	lastCleanup := time.Time{}
	for {
		r.drain(ctx)
		if time.Since(lastCleanup) >= cleanupEvery {
			r.cleanup(ctx)
			lastCleanup = time.Now()
		}
		r.reportLag(ctx)
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-poll.C:
		}
	}
}

// drain relays batches until nothing due is left or a batch fails.
func (r *Relay) drain(ctx context.Context) {
	for ctx.Err() == nil {
		n, err := r.RelayOnce(ctx)
		if err != nil {
			slog.WarnContext(ctx, "outbox relay batch failed", logging.AttrComponent, "outbox", logging.AttrError, err)
			return
		}
		if n < r.cfg.BatchSize {
			return
		}
	}
}

type row struct {
	id        string
	kind      string
	payload   []byte
	carrier   []byte
	requestID sql.NullString
	attempts  int
}

// RelayOnce claims one batch of due rows, delivers each and records the
// outcome. It returns how many rows it claimed.
func (r *Relay) RelayOnce(ctx context.Context) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := claim(ctx, tx, r.cfg.BatchSize)
	if err != nil {
		return 0, err
	}
	for _, rw := range rows {
		jobID, sendErr := r.send(ctx, rw)
		if err := record(ctx, tx, rw, jobID, sendErr); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return len(rows), nil
}

func claim(ctx context.Context, tx *sql.Tx, limit int) ([]row, error) {
	rs, err := tx.QueryContext(ctx, `
		SELECT id, kind, payload, trace_carrier, request_id, attempts
		FROM job_outbox
		WHERE sent_at IS NULL AND next_attempt_at <= now()
		ORDER BY next_attempt_at, created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	defer func() { _ = rs.Close() }()
	var out []row
	for rs.Next() {
		var rw row
		if err := rs.Scan(&rw.id, &rw.kind, &rw.payload, &rw.carrier, &rw.requestID, &rw.attempts); err != nil {
			return nil, fmt.Errorf("claim: scan: %w", err)
		}
		out = append(out, rw)
	}
	return out, rs.Err()
}

// send delivers one row in a span that continues the enqueuing request's
// trace; the gRPC client carries that trace on to river-service.
func (r *Relay) send(ctx context.Context, rw row) (int64, error) {
	parent := ctx
	if len(rw.carrier) > 0 {
		var carrier propagation.MapCarrier
		if json.Unmarshal(rw.carrier, &carrier) == nil {
			parent = otel.GetTextMapPropagator().Extract(ctx, carrier)
		}
	}
	if rw.requestID.Valid {
		parent = logging.WithRequestID(parent, rw.requestID.String)
	}
	ctx, span := r.tracer.Start(parent, "outbox.relay "+rw.kind,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(attribute.String("job.kind", rw.kind), attribute.Int("outbox.attempts", rw.attempts)))
	defer span.End()

	req := &jobsv1.EnqueueRequest{}
	if err := protojson.Unmarshal(rw.payload, req); err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("decode payload: %w", err)
	}
	req.IdempotencyKey = rw.id
	req.RequestId = rw.requestID.String

	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	res, err := r.sender.Enqueue(sendCtx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return 0, err
	}
	return res.GetJobId(), nil
}

func record(ctx context.Context, tx *sql.Tx, rw row, jobID int64, sendErr error) error {
	if sendErr == nil {
		relaySent.Add(1)
		_, err := tx.ExecContext(ctx, `UPDATE job_outbox SET sent_at = now(), job_id = $2, last_error = NULL WHERE id = $1`, rw.id, jobID)
		return wrap("mark sent", err)
	}
	relayFailed.Add(1)
	slog.WarnContext(ctx, "outbox delivery failed; will retry", logging.AttrComponent, "outbox",
		"job_kind", rw.kind, "outbox_id", rw.id, "attempts", rw.attempts+1, logging.AttrError, sendErr)
	_, err := tx.ExecContext(ctx, `
		UPDATE job_outbox
		SET attempts = attempts + 1, last_error = $2, next_attempt_at = now() + $3 * interval '1 millisecond'
		WHERE id = $1`, rw.id, sendErr.Error(), backoff(rw.attempts+1).Milliseconds())
	return wrap("record failure", err)
}

// backoff is the wait before the given attempt: 1s doubling, capped at
// maxBackoff.
func backoff(attempt int) time.Duration {
	d := time.Second
	for i := 1; i < attempt && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

func (r *Relay) cleanup(ctx context.Context) {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM job_outbox WHERE sent_at < now() - $1 * interval '1 second'`, int64(r.cfg.Retention.Seconds())); err != nil {
		slog.WarnContext(ctx, "outbox cleanup failed", logging.AttrComponent, "outbox", logging.AttrError, err)
	}
}

func (r *Relay) reportLag(ctx context.Context) {
	var pending int64
	var lag sql.NullFloat64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*), EXTRACT(EPOCH FROM now() - min(created_at)) FROM job_outbox WHERE sent_at IS NULL`).Scan(&pending, &lag); err != nil {
		return
	}
	relayPending.Set(pending)
	relayLagSeconds.Set(int64(lag.Float64))
}

// listen signals wake on every committed outbox write, reconnecting when the
// connection drops. Polling covers any gap.
func (r *Relay) listen(ctx context.Context, wake chan<- struct{}) {
	for ctx.Err() == nil {
		err := r.listenOnce(ctx, wake)
		if ctx.Err() != nil {
			return
		}
		slog.WarnContext(ctx, "outbox listen connection lost; polling until it reconnects",
			logging.AttrComponent, "outbox", logging.AttrError, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(relistenAfter):
		}
	}
}

func (r *Relay) listenOnce(ctx context.Context, wake chan<- struct{}) error {
	conn, err := pgx.Connect(ctx, r.cfg.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(fmt.Errorf("outbox: %s", op), err)
}
