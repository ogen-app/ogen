package outbox_test

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	jobsv1 "github.com/ogen-app/ogen/gen/jobs/v1"
	"github.com/ogen-app/ogen/src/jobs/outbox"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/pgtest"
)

func emailReq(user string) *jobsv1.EnqueueRequest {
	return &jobsv1.EnqueueRequest{Args: &jobsv1.EnqueueRequest_SendEmail{SendEmail: &jobsv1.SendEmailArgs{UserId: user}}}
}

func pending(t *testing.T, db *bun.DB) int {
	t.Helper()
	var n int
	if err := db.DB.QueryRowContext(t.Context(), `SELECT count(*) FROM job_outbox WHERE sent_at IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWriteTxCommitsWithTheTransaction(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	w := outbox.NewWriter(db.DB)

	errRollback := errors.New("business write failed")
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := w.WriteTx(ctx, tx.Tx, "send_email", emailReq("u1")); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("RunInTx: %v", err)
	}
	if n := pending(t, db); n != 0 {
		t.Fatalf("rolled-back write left %d rows, want 0", n)
	}

	if err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return w.WriteTx(ctx, tx.Tx, "send_email", emailReq("u1"))
	}); err != nil {
		t.Fatal(err)
	}
	if n := pending(t, db); n != 1 {
		t.Fatalf("committed write left %d rows, want 1", n)
	}
}

// fakeSender records what the relay delivers and fails while err is set.
type fakeSender struct {
	mu     sync.Mutex
	err    error
	reqs   []*jobsv1.EnqueueRequest
	traces []trace.TraceID
}

func (f *fakeSender) Enqueue(ctx context.Context, req *jobsv1.EnqueueRequest) (*jobsv1.EnqueueResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	f.traces = append(f.traces, trace.SpanContextFromContext(ctx).TraceID())
	if f.err != nil {
		return nil, f.err
	}
	return &jobsv1.EnqueueResponse{JobId: int64(len(f.reqs))}, nil
}

func TestRelayDeliversAndContinuesTheTrace(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, span := tp.Tracer("test").Start(logging.WithRequestID(t.Context(), "req-9"), "schedule post")
	at := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	req := emailReq("u1")
	req.ScheduledAt = timestamppb.New(at)
	if err := outbox.NewWriter(db.DB).Write(ctx, "send_email", req); err != nil {
		t.Fatal(err)
	}
	span.End()

	sender := &fakeSender{}
	relay := outbox.NewRelay(db.DB, sender, outbox.RelayConfig{})
	n, err := relay.RelayOnce(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("RelayOnce = %d, %v; want 1 row", n, err)
	}
	got := sender.reqs[0]
	if got.GetIdempotencyKey() == "" || got.GetRequestId() != "req-9" || got.GetSendEmail().GetUserId() != "u1" {
		t.Errorf("delivered %v", got)
	}
	if !got.GetScheduledAt().AsTime().Equal(at) {
		t.Errorf("scheduled_at = %v, want %v", got.GetScheduledAt().AsTime(), at)
	}
	if sender.traces[0] != span.SpanContext().TraceID() {
		t.Errorf("relay trace = %s, want the enqueuing trace %s", sender.traces[0], span.SpanContext().TraceID())
	}

	var jobID int64
	if err := db.DB.QueryRowContext(t.Context(), `SELECT job_id FROM job_outbox WHERE id = $1 AND sent_at IS NOT NULL`, got.GetIdempotencyKey()).Scan(&jobID); err != nil || jobID != 1 {
		t.Fatalf("sent row job_id = %d, %v", jobID, err)
	}
	if n, _ := relay.RelayOnce(t.Context()); n != 0 {
		t.Errorf("second RelayOnce claimed %d rows, want 0", n)
	}
}

func TestRelayKeepsRowsRiverServiceRefuses(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	if err := outbox.NewWriter(db.DB).Write(t.Context(), "send_email", emailReq("u1")); err != nil {
		t.Fatal(err)
	}

	sender := &fakeSender{err: status.Error(codes.Unimplemented, "job kind \"send_email\" is not supported")}
	relay := outbox.NewRelay(db.DB, sender, outbox.RelayConfig{})
	if n, err := relay.RelayOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("RelayOnce = %d, %v", n, err)
	}
	var attempts int
	var lastErr string
	var wait float64
	if err := db.DB.QueryRowContext(t.Context(), `
		SELECT attempts, last_error, EXTRACT(EPOCH FROM next_attempt_at - now())::float8
		FROM job_outbox WHERE sent_at IS NULL`).Scan(&attempts, &lastErr, &wait); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastErr == "" || wait <= 0 {
		t.Errorf("refused row: attempts %d, last_error %q, retry in %.1fs; want a backed-off retry", attempts, lastErr, wait)
	}
	// Not due yet, so a second pass leaves it alone.
	if n, _ := relay.RelayOnce(t.Context()); n != 0 {
		t.Errorf("RelayOnce before the backoff claimed %d rows, want 0", n)
	}
}

// TestRelayStopsAtTheFirstFailedSend checks an unreachable river-service costs
// one send per poll, not one per claimed row: the failed row is backed off and
// the rest stay due for the next poll.
func TestRelayStopsAtTheFirstFailedSend(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	w := outbox.NewWriter(db.DB)
	for _, u := range []string{"u1", "u2", "u3"} {
		if err := w.Write(t.Context(), "send_email", emailReq(u)); err != nil {
			t.Fatal(err)
		}
	}

	sender := &fakeSender{err: status.Error(codes.Unavailable, "connection refused")}
	relay := outbox.NewRelay(db.DB, sender, outbox.RelayConfig{})
	if n, err := relay.RelayOnce(t.Context()); err != nil || n != 1 || len(sender.reqs) != 1 {
		t.Fatalf("RelayOnce = %d, %v after %d sends; want one send, then stop", n, err, len(sender.reqs))
	}

	sender.err = nil
	if n, err := relay.RelayOnce(t.Context()); err != nil || n != 2 {
		t.Fatalf("next poll = %d, %v; want the 2 untouched rows", n, err)
	}
	if p := pending(t, db); p != 1 {
		t.Errorf("pending = %d, want only the backed-off row", p)
	}
}

func TestConcurrentRelaysDeliverEachRowOnce(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	w := outbox.NewWriter(db.DB)
	const rows = 30
	for i := range rows {
		if err := w.Write(t.Context(), "send_email", emailReq(string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	sender := &fakeSender{}
	var wg sync.WaitGroup
	for range 3 {
		relay := outbox.NewRelay(db.DB, sender, outbox.RelayConfig{BatchSize: 5})
		wg.Go(func() {
			for {
				n, err := relay.RelayOnce(t.Context())
				if err != nil {
					t.Errorf("RelayOnce: %v", err)
					return
				}
				if n == 0 {
					return
				}
			}
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, r := range sender.reqs {
		if seen[r.GetIdempotencyKey()] {
			t.Fatalf("row %s delivered twice", r.GetIdempotencyKey())
		}
		seen[r.GetIdempotencyKey()] = true
	}
	if len(seen) != rows {
		t.Errorf("delivered %d rows, want %d", len(seen), rows)
	}
}

// TestRelayWakesOnCommit checks the LISTEN path: with polling effectively off,
// a committed write is delivered within moments.
func TestRelayWakesOnCommit(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	var name string
	if err := db.DB.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(pgtest.BaseDSN())
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name

	sender := &fakeSender{}
	relay := outbox.NewRelay(db.DB, sender, outbox.RelayConfig{DSN: u.String(), PollInterval: time.Hour})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		relay.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Run drains once at start; give the listener time to connect.
	time.Sleep(500 * time.Millisecond)
	if err := outbox.NewWriter(db.DB).Write(t.Context(), "send_email", emailReq("u-wake")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sender.mu.Lock()
		n := len(sender.reqs)
		sender.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("committed write was not delivered without a poll")
}
