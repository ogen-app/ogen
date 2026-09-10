package usage_test

import (
	"context"
	"expvar"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// Test vendors are registered once for this package's test binary. The vendor
// registry is process-global, but Go gives each package its own test binary,
// so this does not collide with vendor's own registry tests.
func init() {
	vendors.Register(vendors.Descriptor{
		Name:   "test-model",
		Family: vendors.FamilyModel,
		Prices: vendors.PriceTable{
			Version: "test-v1",
			Models: map[string]vendors.Rates{
				"m1": {vendors.KindInput: 3_000_000, vendors.KindOutput: 15_000_000},
			},
		},
	})
	// A count-only publisher (empty price table) — cost 0, not an unknown-model gap.
	vendors.Register(vendors.Descriptor{
		Name:    "test-publisher",
		Family:  vendors.FamilyPublisher,
		Metered: true,
		Prices:  vendors.PriceTable{Version: "count-only"},
	})
	// Dedicated vendor for the price-override test, so mutating its prices via
	// ApplyModelPrices doesn't pollute the shared "test-model".
	vendors.Register(vendors.Descriptor{
		Name:   "test-pricing",
		Family: vendors.FamilyModel,
		Prices: vendors.PriceTable{Version: "base", Models: map[string]vendors.Rates{
			"keep": {vendors.KindInput: 1_000_000},
		}},
	})
}

type fakeWriter struct {
	mu     sync.Mutex
	events []*models.UsageEvent
	block  chan struct{} // when non-nil, Insert waits on it before returning
	err    error
}

func (w *fakeWriter) Insert(_ context.Context, events []*models.UsageEvent) error {
	if w.block != nil {
		<-w.block
	}
	if w.err != nil {
		return w.err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, events...)
	return nil
}

func (w *fakeWriter) all() []*models.UsageEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*models.UsageEvent(nil), w.events...)
}

func testMetrics() *usage.Metrics {
	return &usage.Metrics{
		EventsRecorded:      new(expvar.Int),
		EventsDropped:       new(expvar.Int),
		WriteErrors:         new(expvar.Int),
		UnknownModel:        new(expvar.Int),
		UnknownVendor:       new(expvar.Int),
		LimitBlocks:         new(expvar.Int),
		LimitWarnings:       new(expvar.Int),
		EnforcementDegraded: new(expvar.Int),
	}
}

func tenantCtx(id string) context.Context {
	return tenantctx.With(context.Background(), id)
}

func closeRecorder(t *testing.T, r *usage.Recorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("recorder Close: %v", err)
	}
}

func TestRecorder_RecordsPricedEvent(t *testing.T) {
	w := &fakeWriter{}
	m := testMetrics()
	r := usage.NewRecorder(w, m, usage.Config{})

	r.Record(tenantCtx("tenant-A"), "test-model", "content_plan", vendors.MeterEvent{
		Model:     "m1",
		Operation: "generate",
		Usage:     vendors.Usage{vendors.KindInput: 1_000_000, vendors.KindOutput: 0, vendors.KindReasoning: 50},
	})
	closeRecorder(t, r)

	got := w.all()
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	e := got[0]
	if e.TenantID != "tenant-A" {
		t.Errorf("TenantID = %q, want tenant-A", e.TenantID)
	}
	if e.Vendor != "test-model" || e.Family != "model" || e.Feature != "content_plan" {
		t.Errorf("dimensions = %q/%q/%q", e.Vendor, e.Family, e.Feature)
	}
	if e.InputTokens != 1_000_000 {
		t.Errorf("InputTokens = %d, want 1000000", e.InputTokens)
	}
	if e.ExtraUnits["reasoning"] != 50 {
		t.Errorf("ExtraUnits[reasoning] = %d, want 50", e.ExtraUnits["reasoning"])
	}
	// 1e6 input * 3e6 / 1e6 = 3_000_000; output 0; reasoning unpriced.
	if e.CostMicros != 3_000_000 {
		t.Errorf("CostMicros = %d, want 3000000", e.CostMicros)
	}
	if e.PriceVersion != "test-v1" {
		t.Errorf("PriceVersion = %q, want test-v1", e.PriceVersion)
	}
	if e.ID == "" {
		t.Error("ID not generated")
	}
	if m.EventsRecorded.Value() != 1 {
		t.Errorf("EventsRecorded = %d, want 1", m.EventsRecorded.Value())
	}
}

func TestRecorder_UnknownVendorSkipped(t *testing.T) {
	w := &fakeWriter{}
	m := testMetrics()
	r := usage.NewRecorder(w, m, usage.Config{})

	r.Record(tenantCtx("t"), "ghost", "content_plan", vendors.MeterEvent{Model: "x", Usage: vendors.Usage{vendors.KindInput: 100}})
	closeRecorder(t, r)

	if len(w.all()) != 0 {
		t.Errorf("expected no events for unknown vendor, got %d", len(w.all()))
	}
	if m.UnknownVendor.Value() != 1 {
		t.Errorf("UnknownVendor = %d, want 1", m.UnknownVendor.Value())
	}
}

func TestRecorder_UnknownModelRecordedZeroCost(t *testing.T) {
	w := &fakeWriter{}
	m := testMetrics()
	r := usage.NewRecorder(w, m, usage.Config{})

	r.Record(tenantCtx("t"), "test-model", "content_plan", vendors.MeterEvent{
		Model: "not-priced", Operation: "generate", Usage: vendors.Usage{vendors.KindInput: 100},
	})
	closeRecorder(t, r)

	got := w.all()
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 (unknown model still recorded)", len(got))
	}
	if got[0].CostMicros != 0 {
		t.Errorf("CostMicros = %d, want 0 for unpriced model", got[0].CostMicros)
	}
	if m.UnknownModel.Value() != 1 {
		t.Errorf("UnknownModel = %d, want 1", m.UnknownModel.Value())
	}
}

func TestRecorder_CountOnlyPublisherEvent(t *testing.T) {
	w := &fakeWriter{}
	m := testMetrics()
	r := usage.NewRecorder(w, m, usage.Config{})

	r.Record(tenantCtx("t"), "test-publisher", "auto_publish", vendors.MeterEvent{
		Model:           "instagram",
		Operation:       "publish",
		Usage:           vendors.Usage{vendors.KindPost: 1},
		Platform:        "instagram",
		SocialAccountID: "acc-1",
	})
	closeRecorder(t, r)

	got := w.all()
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	e := got[0]
	if e.Family != "publisher" || e.CostMicros != 0 {
		t.Errorf("family=%q cost=%d, want publisher/0", e.Family, e.CostMicros)
	}
	if e.Platform != "instagram" || e.SocialAccountID != "acc-1" {
		t.Errorf("platform=%q social_account_id=%q", e.Platform, e.SocialAccountID)
	}
	if e.ExtraUnits["post"] != 1 {
		t.Errorf("ExtraUnits[post]=%d, want 1", e.ExtraUnits["post"])
	}
	// Count-only is intentional, not an unknown-model gap.
	if m.UnknownModel.Value() != 0 {
		t.Errorf("UnknownModel=%d, want 0", m.UnknownModel.Value())
	}
}

func TestRecorder_UntenantedSkipped(t *testing.T) {
	w := &fakeWriter{}
	m := testMetrics()
	r := usage.NewRecorder(w, m, usage.Config{})

	r.Record(t.Context(), "test-model", "content_plan", vendors.MeterEvent{Model: "m1", Usage: vendors.Usage{vendors.KindInput: 100}})
	closeRecorder(t, r)

	if len(w.all()) != 0 {
		t.Errorf("expected no events for untenanted call, got %d", len(w.all()))
	}
}

func TestRecorder_NilSafe(t *testing.T) {
	var r *usage.Recorder
	r.Record(tenantCtx("t"), "test-model", "content_plan", vendors.MeterEvent{Model: "m1"})
	if err := r.Close(t.Context()); err != nil {
		t.Errorf("nil Close = %v, want nil", err)
	}
}

func TestRecorder_DropsOnFullBuffer(t *testing.T) {
	block := make(chan struct{})
	w := &fakeWriter{block: block}
	m := testMetrics()
	// Tiny buffer + batch of 1 so the first event pins the writer (blocked)
	// and the rest overflow.
	r := usage.NewRecorder(w, m, usage.Config{BufferSize: 1, BatchSize: 1, FlushEvery: time.Hour})

	for range 50 {
		r.Record(tenantCtx("t"), "test-model", "content_plan", vendors.MeterEvent{Model: "m1", Usage: vendors.Usage{vendors.KindInput: 1}})
	}
	if m.EventsDropped.Value() == 0 {
		close(block)
		closeRecorder(t, r)
		t.Fatal("expected some events dropped while writer blocked, got 0")
	}

	close(block) // unblock the writer so Close can drain
	closeRecorder(t, r)
}

func TestRecorder_BatchesAndRecordsAll(t *testing.T) {
	w := &fakeWriter{}
	m := testMetrics()
	r := usage.NewRecorder(w, m, usage.Config{BufferSize: 100, BatchSize: 10, FlushEvery: time.Hour})

	const n = 25
	for range n {
		r.Record(tenantCtx("t"), "test-model", "content_plan", vendors.MeterEvent{Model: "m1", Operation: "generate", Usage: vendors.Usage{vendors.KindInput: 1}})
	}
	closeRecorder(t, r)

	if got := len(w.all()); got != n {
		t.Errorf("recorded %d events, want %d", got, n)
	}
	if m.EventsRecorded.Value() != n {
		t.Errorf("EventsRecorded = %d, want %d", m.EventsRecorded.Value(), n)
	}
}
