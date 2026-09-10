package usage

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Writer is the narrow persistence surface the Recorder needs;
// repository.UsageRepository satisfies it.
type Writer interface {
	Insert(ctx context.Context, events []*models.UsageEvent) error
}

// Config tunes the Recorder's buffer and batching.
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

// Recorder captures usage events and writes them to the analytics DB on a
// background goroutine, so a model/publisher call never waits on — or fails
// from — analytics (CON-86 FR5). Cost and tenant are snapshotted at enqueue
// time; only the DB write is deferred.
//
// A nil *Recorder is a valid no-op: when ANALYTICS_DSN is empty the server
// holds a nil Recorder and every Record call returns immediately, recording
// nothing (CON-86 FR10).
type Recorder struct {
	writer  Writer
	metrics *Metrics

	ch   chan *models.UsageEvent
	done chan struct{}
	wg   sync.WaitGroup

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
		ch:           make(chan *models.UsageEvent, cfg.BufferSize),
		done:         make(chan struct{}),
		batchSize:    cfg.BatchSize,
		flushEvery:   cfg.FlushEvery,
		writeTimeout: cfg.WriteTimeout,
		now:          func() time.Time { return time.Now().UTC() },
	}
	r.wg.Go(r.loop)
	return r
}

// Record builds a usage event from a vendor MeterEvent and enqueues it for an
// async write. Tenant is resolved from ctx now; an untenanted call is not
// attributed and is skipped (CON-86 §9). Cost is snapshotted now from the
// registry. Enqueue never blocks: a full buffer drops the event (counted),
// because analytics must never add latency to the caller.
func (r *Recorder) Record(ctx context.Context, vendorName, feature string, ev vendors.MeterEvent) {
	if r == nil {
		return
	}
	tenantID, ok := tenantctx.From(ctx)
	if !ok {
		return // system/untenanted calls aren't attributed
	}

	desc, known := vendors.Get(vendorName)
	if !known {
		// An unregistered vendor has no family/pricing — a programming error,
		// not a data point. Count and drop.
		r.metrics.UnknownVendor.Add(1)
		return
	}

	micros, priced := costFor(desc, ev.Model, ev.Usage)
	if !priced {
		r.metrics.UnknownModel.Add(1) // cost stays 0; the event is still recorded
	}

	id, err := models.NewID()
	if err != nil {
		r.metrics.EventsDropped.Add(1)
		return
	}

	event := toEvent(id, tenantID, vendorName, string(desc.Family), feature, ev, micros, desc.Prices.Version, r.now())

	select {
	case r.ch <- event:
	default:
		r.metrics.EventsDropped.Add(1)
	}
}

// RecordResp is the call-site convenience: it runs the vendor's Meter over a
// provider response (e.g. a genkit *ai.ModelResponse, or an embed-usage
// struct) to obtain the operation + unit breakdown, then records it with the
// caller-supplied model id and feature. The genkit dependency stays inside
// the Meter implementation — this package never imports it. Nil-safe.
func (r *Recorder) RecordResp(ctx context.Context, vendorName, model, feature string, resp any) {
	if r == nil {
		return
	}
	desc, known := vendors.Get(vendorName)
	if !known {
		r.metrics.UnknownVendor.Add(1)
		return
	}
	if desc.Meter == nil {
		return
	}
	op, u, ok := desc.Meter.Extract(resp)
	if !ok {
		return // nothing to record (empty/unrecognised response)
	}
	r.Record(ctx, vendorName, feature, vendors.MeterEvent{Model: model, Operation: op, Usage: u})
}

// Close stops the writer, draining queued events first. It returns when the
// drain completes or ctx is cancelled (whichever is first), so shutdown can
// bound how long it waits on analytics.
func (r *Recorder) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	close(r.done)
	stopped := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Recorder) loop() {
	ticker := time.NewTicker(r.flushEvery)
	defer ticker.Stop()

	batch := make([]*models.UsageEvent, 0, r.batchSize)
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

func (r *Recorder) flush(batch []*models.UsageEvent) {
	// Bound the batch write so a stalled analytics DB can't hang the loop
	// goroutine forever (which would also leak it past Close).
	ctx, cancel := context.WithTimeout(context.Background(), r.writeTimeout)
	defer cancel()
	// Writes are intentionally untenanted-but-system: each row already carries
	// its tenant_id (set at Record time), which BeforeAppendModel preserves in
	// a system context (models/tenant_scoped.go).
	if err := r.writer.Insert(tenantctx.WithSystem(ctx), batch); err != nil {
		r.metrics.WriteErrors.Add(1)
		slog.ErrorContext(ctx, "write batch failed", logging.AttrComponent, "usage", "count", len(batch), logging.AttrError, err)
		return
	}
	r.metrics.EventsRecorded.Add(int64(len(batch)))
}

// costFor computes the snapshot cost for ev under the descriptor's price
// table. An empty price table is "count-only" (e.g. a publisher we count but
// don't price): cost 0 is expected, not a gap. When the table HAS models but
// this one is missing, priced is false so the caller counts an unknown-model
// miss (CON-86 FR3).
func costFor(d vendors.Descriptor, model string, u vendors.Usage) (micros int64, priced bool) {
	if len(d.Prices.Models) == 0 {
		return 0, true
	}
	rates, ok := d.Prices.Models[model]
	if !ok {
		return 0, false
	}
	return vendors.Cost(u, rates), true
}

// toEvent maps a vendor MeterEvent onto the persisted row, splitting the
// canonical Usage map into the broken-out token columns and the extra_units
// jsonb (the long tail: reasoning, post, api_call, account, …).
func toEvent(id, tenantID, vendorName, family, feature string, ev vendors.MeterEvent, micros int64, version string, now time.Time) *models.UsageEvent {
	e := &models.UsageEvent{
		ID:              id,
		Vendor:          vendorName,
		Family:          family,
		Model:           ev.Model,
		Operation:       ev.Operation,
		Feature:         feature,
		Platform:        ev.Platform,
		SocialAccountID: ev.SocialAccountID,
		CostMicros:      micros,
		PriceVersion:    version,
		OccurredAt:      now,
	}
	e.TenantID = tenantID // preserved by BeforeAppendModel in the system write ctx

	for kind, n := range ev.Usage {
		switch kind {
		case vendors.KindInput:
			e.InputTokens = n
		case vendors.KindOutput:
			e.OutputTokens = n
		case vendors.KindCacheRead:
			e.CacheReadTokens = n
		case vendors.KindCacheCreation:
			e.CacheCreationTokens = n
		default:
			if n != 0 {
				if e.ExtraUnits == nil {
					e.ExtraUnits = make(map[string]int64)
				}
				e.ExtraUnits[string(kind)] = n
			}
		}
	}
	return e
}
