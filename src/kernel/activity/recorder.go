package activity

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Writer is the narrow persistence surface the Recorder needs;
// repository.ActivityRepository satisfies it.
type Writer interface {
	Insert(ctx context.Context, events []*models.ActivityEvent) error
}

// Config tunes the Recorder's buffer and batching. Zero values take defaults.
type Config struct {
	BufferSize   int           // bounded queue depth; overflow drops + counts
	BatchSize    int           // max rows per insert
	FlushEvery   time.Duration // max latency before a partial batch is flushed
	WriteTimeout time.Duration // per-batch Insert deadline; bounds a stalled DB
}

func (c Config) withDefaults() Config {
	if c.BufferSize <= 0 {
		c.BufferSize = 1024
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.FlushEvery <= 0 {
		c.FlushEvery = time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 30 * time.Second
	}
	return c
}

// Recorder captures activity events and writes them to the analytics DB on a
// background goroutine, so a request/flow never waits on — or fails from —
// activity collection (CON-125). Tenant + user are snapshotted at enqueue time;
// only the DB write is deferred.
//
// A nil *Recorder is a valid no-op: when ANALYTICS_DSN is empty the server
// holds a nil Recorder and every Record call returns immediately, recording
// nothing. This keeps every call-site unconditional.
type Recorder struct {
	writer  Writer
	metrics *Metrics

	ch   chan *models.ActivityEvent
	done chan struct{}
	// stopped is closed once the loop goroutine has fully drained and exited.
	// closeOnce guards the single close(done) + waiter launch so Close is safe
	// to call repeatedly or concurrently (e.g. a retried shutdown after an
	// earlier ctx-timeout) without panicking on a double close.
	stopped   chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	batchSize    int
	flushEvery   time.Duration
	writeTimeout time.Duration

	now func() time.Time // injectable clock for tests
}

// NewRecorder starts the background writer. Call Close to drain and stop.
func NewRecorder(w Writer, m *Metrics, cfg Config) *Recorder {
	cfg = cfg.withDefaults()
	r := &Recorder{
		writer:       w,
		metrics:      m,
		ch:           make(chan *models.ActivityEvent, cfg.BufferSize),
		done:         make(chan struct{}),
		stopped:      make(chan struct{}),
		batchSize:    cfg.BatchSize,
		flushEvery:   cfg.FlushEvery,
		writeTimeout: cfg.WriteTimeout,
		now:          func() time.Time { return time.Now().UTC() },
	}
	r.wg.Go(r.loop)
	return r
}

// Record builds an activity event and enqueues it for an async write. Tenant is
// resolved from ctx now; an untenanted, non-system call is not attributed and
// is skipped. The acting user is taken from ctx (the logging user id set by the
// auth middleware) unless WithUser overrides it. Enqueue never blocks: a full
// buffer drops the event (counted), because collection must never add latency
// to the caller.
func (r *Recorder) Record(ctx context.Context, category, typ string, opts ...Option) {
	if r == nil {
		return
	}
	tenantID, ok := tenantctx.From(ctx)
	if !ok {
		return // system/untenanted calls aren't attributed
	}

	id, err := models.NewID()
	if err != nil {
		r.metrics.EventsDropped.Add(1)
		return
	}

	event := &models.ActivityEvent{
		ID:         id,
		Category:   category,
		Type:       typ,
		OccurredAt: r.now(),
	}
	event.TenantID = tenantID // preserved by BeforeAppendModel in the system write ctx
	if uid, ok := logging.UserIDFrom(ctx); ok {
		event.UserID = uid
	}
	for _, opt := range opts {
		opt(event)
	}

	select {
	case r.ch <- event:
	default:
		r.metrics.EventsDropped.Add(1)
	}
}

// Close stops the writer, draining queued events first. It returns when the
// drain completes or ctx is cancelled (whichever is first), so shutdown can
// bound how long it waits on activity collection.
//
// Close is idempotent and safe under concurrent callers: closeOnce guards the
// single close(done) and launches the one waiter that closes the shared stopped
// channel, so a repeated or racing Close (e.g. a shutdown retried after an
// earlier ctx-timeout) never double-closes. Every caller then waits on the same
// stopped channel or returns ctx.Err() on cancellation.
func (r *Recorder) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		close(r.done)
		go func() {
			r.wg.Wait()
			close(r.stopped)
		}()
	})
	select {
	case <-r.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Recorder) loop() {
	ticker := time.NewTicker(r.flushEvery)
	defer ticker.Stop()

	batch := make([]*models.ActivityEvent, 0, r.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		r.flush(batch)
		batch = batch[:0]
	}

	for {
		select {
		case e := <-r.ch:
			batch = append(batch, e)
			if len(batch) >= r.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-r.done:
			// Drain whatever is still queued, then exit.
			for {
				select {
				case e := <-r.ch:
					batch = append(batch, e)
					if len(batch) >= r.batchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (r *Recorder) flush(batch []*models.ActivityEvent) {
	// Bound the batch write so a stalled analytics DB can't hang the loop
	// goroutine forever (which would also leak it past Close).
	ctx, cancel := context.WithTimeout(context.Background(), r.writeTimeout)
	defer cancel()
	// Writes are intentionally untenanted-but-system: each row already carries
	// its tenant_id (set at Record time), which BeforeAppendModel preserves in
	// a system context (models/tenant_scoped.go).
	if err := r.writer.Insert(tenantctx.WithSystem(ctx), batch); err != nil {
		r.metrics.WriteErrors.Add(1)
		slog.ErrorContext(ctx, "write batch failed", logging.AttrComponent, "activity", "count", len(batch), logging.AttrError, err)
		return
	}
	r.metrics.EventsRecorded.Add(int64(len(batch)))
}
