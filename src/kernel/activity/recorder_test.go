package activity_test

import (
	"context"
	"expvar"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

type fakeWriter struct {
	mu     sync.Mutex
	events []*models.ActivityEvent
	block  chan struct{} // when non-nil, Insert waits on it before returning
}

func (w *fakeWriter) Insert(_ context.Context, events []*models.ActivityEvent) error {
	if w.block != nil {
		<-w.block
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, events...)
	return nil
}

func (w *fakeWriter) all() []*models.ActivityEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*models.ActivityEvent(nil), w.events...)
}

func testMetrics() *activity.Metrics {
	return &activity.Metrics{
		EventsRecorded: new(expvar.Int),
		EventsDropped:  new(expvar.Int),
		WriteErrors:    new(expvar.Int),
	}
}

func closeRecorder(t *testing.T, r *activity.Recorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("recorder Close: %v", err)
	}
}

func TestRecorder_RecordsEvent(t *testing.T) {
	w := &fakeWriter{}
	m := testMetrics()
	r := activity.NewRecorder(w, m, activity.Config{FlushEvery: 5 * time.Millisecond})

	ctx := logging.WithUserID(tenantctx.With(context.Background(), "tenant-1"), "user-9")
	r.Record(ctx, "post", "post_created",
		activity.WithEntity("post", "p123"),
		activity.WithStatus("success"),
		activity.WithSource(activity.SourceAPI),
		activity.WithTags("draft"),
		activity.WithPayload(map[string]any{"platform": "instagram"}),
	)
	closeRecorder(t, r)

	got := w.all()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	e := got[0]
	if e.TenantID != "tenant-1" || e.UserID != "user-9" {
		t.Errorf("tenant/user = %q/%q, want tenant-1/user-9", e.TenantID, e.UserID)
	}
	if e.Category != "post" || e.Type != "post_created" {
		t.Errorf("category/type = %q/%q", e.Category, e.Type)
	}
	if e.EntityType != "post" || e.EntityID != "p123" {
		t.Errorf("entity = %q/%q", e.EntityType, e.EntityID)
	}
	if e.Status != "success" || e.Source != activity.SourceAPI {
		t.Errorf("status/source = %q/%q", e.Status, e.Source)
	}
	if len(e.Tags) != 1 || e.Tags[0] != "draft" {
		t.Errorf("tags = %v", e.Tags)
	}
	if e.Payload["platform"] != "instagram" {
		t.Errorf("payload = %v", e.Payload)
	}
	if e.ID == "" || e.OccurredAt.IsZero() {
		t.Errorf("id/occurred_at not set: %q / %v", e.ID, e.OccurredAt)
	}
	if m.EventsRecorded.Value() != 1 {
		t.Errorf("EventsRecorded = %d, want 1", m.EventsRecorded.Value())
	}
}

func TestRecorder_SkipsUntenanted(t *testing.T) {
	w := &fakeWriter{}
	m := testMetrics()
	r := activity.NewRecorder(w, m, activity.Config{FlushEvery: 5 * time.Millisecond})

	r.Record(context.Background(), "post", "post_created") // no tenant in ctx
	closeRecorder(t, r)

	if n := len(w.all()); n != 0 {
		t.Errorf("events = %d, want 0 (untenanted skipped)", n)
	}
	if m.EventsDropped.Value() != 0 {
		t.Errorf("EventsDropped = %d, want 0 (skip is not a drop)", m.EventsDropped.Value())
	}
}

func TestRecorder_WithUserOverridesContext(t *testing.T) {
	w := &fakeWriter{}
	r := activity.NewRecorder(w, testMetrics(), activity.Config{FlushEvery: 5 * time.Millisecond})

	ctx := logging.WithUserID(tenantctx.With(context.Background(), "t"), "ctx-user")
	r.Record(ctx, "authentication", "signup", activity.WithUser("new-user"))
	closeRecorder(t, r)

	got := w.all()
	if len(got) != 1 || got[0].UserID != "new-user" {
		t.Fatalf("user = %v, want new-user", got)
	}
}

func TestRecorder_NilSafe(t *testing.T) {
	var r *activity.Recorder
	r.Record(context.Background(), "post", "post_created") // must not panic
	if err := r.Close(context.Background()); err != nil {
		t.Errorf("nil Close = %v, want nil", err)
	}
}

func TestRecorder_CloseIdempotent(t *testing.T) {
	r := activity.NewRecorder(&fakeWriter{}, testMetrics(), activity.Config{FlushEvery: 5 * time.Millisecond})

	// Concurrent Close calls must not panic on a double close(done); each
	// returns once the loop has drained.
	const callers = 5
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			errs <- r.Close(ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Close = %v, want nil", err)
		}
	}

	// A further sequential Close after shutdown still returns nil, no panic.
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("post-shutdown Close = %v, want nil", err)
	}
}

func TestRecorder_DropsOnFullBuffer(t *testing.T) {
	w := &fakeWriter{block: make(chan struct{})}
	m := testMetrics()
	// Buffer 1, batch 1: the first event is pulled into the loop and blocks on
	// Insert; the buffer then holds 1 more, and everything beyond is dropped.
	r := activity.NewRecorder(w, m, activity.Config{BufferSize: 1, BatchSize: 1, FlushEvery: time.Hour})

	ctx := tenantctx.With(context.Background(), "t")
	// Give the loop a moment to pull the first event and block in Insert.
	r.Record(ctx, "post", "e0")
	time.Sleep(20 * time.Millisecond)
	for range 50 {
		r.Record(ctx, "post", "flood")
	}

	if m.EventsDropped.Value() == 0 {
		t.Errorf("EventsDropped = 0, want > 0 on a saturated buffer")
	}

	close(w.block) // unblock so Close can drain
	closeRecorder(t, r)
}
