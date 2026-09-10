package eventhub

import (
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// drain reads at most n events from ch with a timeout. Returns the
// events received and whether the channel was closed before n events
// arrived. Used to assert delivery without flaky time.Sleep races.
func drain(t *testing.T, ch <-chan Event, n int, timeout time.Duration) (got []Event, closed bool) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for len(got) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got, true
			}
			got = append(got, ev)
		case <-deadline.C:
			return got, false
		}
	}
	return got, false
}

func TestPublishFanOutToMatchingSubscribers(t *testing.T) {
	h := New(Config{})
	ctx := t.Context()

	chA, unsubA, err := h.Subscribe(ctx, SubscribeOpts{UserID: "alice", Topics: []string{"job:*"}})
	if err != nil {
		t.Fatal(err)
	}
	defer unsubA()

	chB, unsubB, err := h.Subscribe(ctx, SubscribeOpts{UserID: "alice", Topics: []string{"entity:*"}})
	if err != nil {
		t.Fatal(err)
	}
	defer unsubB()

	if err := h.Publish(ctx, Event{Topic: "job:foo", Type: "started", UserID: "alice"}); err != nil {
		t.Fatal(err)
	}

	gotA, _ := drain(t, chA, 1, time.Second)
	if len(gotA) != 1 || gotA[0].Topic != "job:foo" {
		t.Errorf("subscriber A: got %+v, want [job:foo]", gotA)
	}

	// B's filter is entity:*; should NOT receive job:foo.
	select {
	case ev := <-chB:
		t.Errorf("subscriber B received unexpected event %+v", ev)
	case <-time.After(50 * time.Millisecond):
		// expected: no event delivered
	}
}

func TestAllTopicMatchesEverything(t *testing.T) {
	h := New(Config{})
	ctx := t.Context()
	ch, unsub, err := h.Subscribe(ctx, SubscribeOpts{UserID: "alice", Topics: []string{"all"}})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	for _, topic := range []string{"job:foo", "entity:post:abc", "user:bob"} {
		_ = h.Publish(ctx, Event{Topic: topic, UserID: "alice"})
	}
	got, _ := drain(t, ch, 3, time.Second)
	if len(got) != 3 {
		t.Fatalf("expected 3 events on 'all' subscription, got %d", len(got))
	}
}

func TestAuthzFiltersByUserID(t *testing.T) {
	h := New(Config{})
	ctx := t.Context()
	ch, unsub, err := h.Subscribe(ctx, SubscribeOpts{UserID: "alice", Topics: []string{"all"}})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	// Three events: one for alice, one for bob, one public.
	_ = h.Publish(ctx, Event{Topic: "job:foo", UserID: "alice"})
	_ = h.Publish(ctx, Event{Topic: "job:bar", UserID: "bob"})
	_ = h.Publish(ctx, Event{Topic: "job:baz" /* public, no UserID */})

	// Alice should see her own event + the public one, NOT bob's.
	got, _ := drain(t, ch, 2, time.Second)
	if len(got) != 2 {
		t.Fatalf("expected 2 events (alice's + public), got %d: %+v", len(got), got)
	}
	for _, ev := range got {
		if ev.UserID == "bob" {
			t.Errorf("alice received bob's event: %+v", ev)
		}
	}
}

func TestTenantIsolation(t *testing.T) {
	h := New(Config{})
	ctx := t.Context()

	chA, unsubA, err := h.Subscribe(ctx, SubscribeOpts{UserID: "ua", TenantID: "tenant-a", Topics: []string{"all"}})
	if err != nil {
		t.Fatal(err)
	}
	defer unsubA()
	chB, unsubB, err := h.Subscribe(ctx, SubscribeOpts{UserID: "ub", TenantID: "tenant-b", Topics: []string{"all"}})
	if err != nil {
		t.Fatal(err)
	}
	defer unsubB()

	// A tenant-tagged event with no UserID (e.g. a job-published analytics
	// event) must reach only the matching tenant.
	_ = h.Publish(ctx, Event{Topic: "entity:post:x", TenantID: "tenant-a"})
	if got, _ := drain(t, chA, 1, time.Second); len(got) != 1 || got[0].TenantID != "tenant-a" {
		t.Fatalf("tenant A should have received its event, got %+v", got)
	}
	if leaked, _ := drain(t, chB, 1, 200*time.Millisecond); len(leaked) != 0 {
		t.Fatalf("tenant B leaked tenant A's event: %+v", leaked)
	}

	// Publish derives the tenant from a tenant context when not set explicitly.
	_ = h.Publish(tenantctx.With(t.Context(), "tenant-b"), Event{Topic: "entity:post:y"})
	if got, _ := drain(t, chB, 1, time.Second); len(got) != 1 || got[0].TenantID != "tenant-b" {
		t.Fatalf("tenant B should have received the ctx-derived event, got %+v", got)
	}
	if leaked, _ := drain(t, chA, 1, 200*time.Millisecond); len(leaked) != 0 {
		t.Fatalf("tenant A leaked tenant B's ctx-derived event: %+v", leaked)
	}
}

func TestBackpressureDisconnectsSlowSubscriber(t *testing.T) {
	// Tiny buffer + a subscriber that never reads => second publish
	// triggers disconnect. We don't read the channel until after the
	// disconnect, then assert it's closed.
	h := New(Config{BufferSize: 1})
	ctx := t.Context()

	chSlow, unsubSlow, err := h.Subscribe(ctx, SubscribeOpts{UserID: "alice", Topics: []string{"all"}, BufferSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer unsubSlow()

	chFast, unsubFast, err := h.Subscribe(ctx, SubscribeOpts{UserID: "alice", Topics: []string{"all"}, BufferSize: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer unsubFast()

	// Read the fast subscriber's events in a goroutine so it stays empty.
	var fastSeen int
	var fastMu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case _, ok := <-chFast:
				if !ok {
					return
				}
				fastMu.Lock()
				fastSeen++
				fastMu.Unlock()
			case <-time.After(500 * time.Millisecond):
				return
			}
		}
	}()

	// Publish more than the slow buffer can hold.
	for range 5 {
		_ = h.Publish(ctx, Event{Topic: "job:foo", UserID: "alice"})
	}
	<-done

	// Slow subscriber should be disconnected: channel closed; reading
	// any remaining buffered events plus the close.
	closedSeen := false
	for {
		_, ok := <-chSlow
		if !ok {
			closedSeen = true
			break
		}
		// drain any buffered event
	}
	if !closedSeen {
		t.Error("slow subscriber should have been disconnected (channel closed)")
	}

	fastMu.Lock()
	defer fastMu.Unlock()
	if fastSeen != 5 {
		t.Errorf("fast subscriber should have received all 5 events; got %d", fastSeen)
	}
}

func TestUnsubscribeIsIdempotent(t *testing.T) {
	h := New(Config{})
	ctx := t.Context()
	ch, unsub, err := h.Subscribe(ctx, SubscribeOpts{UserID: "alice", Topics: []string{"all"}})
	if err != nil {
		t.Fatal(err)
	}
	unsub()
	unsub() // must not panic

	// Channel should be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be closed after unsubscribe")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("read on closed channel should not block")
	}
}

func TestSubscribeRejectsEmptyTopics(t *testing.T) {
	h := New(Config{})
	_, _, err := h.Subscribe(t.Context(), SubscribeOpts{UserID: "alice"})
	if !errors.Is(err, ErrNoTopics) {
		t.Errorf("expected ErrNoTopics, got %v", err)
	}
}

func TestMaxSubscribersPerUserEvictsOldest(t *testing.T) {
	// CON-286: at the cap the Hub evicts the user's OLDEST subscriber and admits
	// the newcomer, rather than rejecting the newcomer. This keeps a reload from
	// being locked out when older connections have leaked.
	h := New(Config{MaxSubscribersPerUser: 2})
	ctx := t.Context()

	ch1, u1, err := h.Subscribe(ctx, SubscribeOpts{UserID: "alice", Topics: []string{"all"}})
	if err != nil {
		t.Fatal(err)
	}
	defer u1()
	ch2, u2, err := h.Subscribe(ctx, SubscribeOpts{UserID: "alice", Topics: []string{"all"}})
	if err != nil {
		t.Fatal(err)
	}
	defer u2()

	// Third subscribe succeeds (never 429) and evicts the oldest (ch1).
	ch3, u3, err := h.Subscribe(ctx, SubscribeOpts{UserID: "alice", Topics: []string{"all"}})
	if err != nil {
		t.Fatalf("third subscribe should succeed via eviction, got %v", err)
	}
	defer u3()

	// The evicted subscriber's channel is closed — which is exactly what
	// unblocks a parked writer goroutine so it can release its slot.
	if !channelClosed(t, ch1) {
		t.Error("oldest subscriber (ch1) should have been evicted (channel closed)")
	}

	// The user is still capped at 2 live subscribers, not 3.
	if got := h.(*inProcHub).ActiveCount(); got != 2 {
		t.Errorf("active=%d, want 2 (cap held after eviction)", got)
	}

	// The two survivors still receive events.
	_ = h.Publish(ctx, Event{Topic: "job:foo", UserID: "alice"})
	if got, _ := drain(t, ch2, 1, time.Second); len(got) != 1 {
		t.Errorf("surviving ch2 should receive events, got %+v", got)
	}
	if got, _ := drain(t, ch3, 1, time.Second); len(got) != 1 {
		t.Errorf("surviving ch3 should receive events, got %+v", got)
	}

	// A different user is unaffected by alice's cap: bob is admitted and the cap
	// is per-user, so his subscribe evicts none of alice's — active goes to 3.
	_, u4, err := h.Subscribe(ctx, SubscribeOpts{UserID: "bob", Topics: []string{"all"}})
	if err != nil {
		t.Fatal(err)
	}
	defer u4()
	if got := h.(*inProcHub).ActiveCount(); got != 3 {
		t.Errorf("active=%d, want 3 (alice's 2 survivors + bob); alice must not be evicted by bob", got)
	}
	// Alice's survivors are still live after bob joins.
	_ = h.Publish(ctx, Event{Topic: "job:bar", UserID: "alice"})
	if got, _ := drain(t, ch2, 1, time.Second); len(got) != 1 {
		t.Errorf("ch2 should still receive after bob subscribed, got %+v", got)
	}
	if got, _ := drain(t, ch3, 1, time.Second); len(got) != 1 {
		t.Errorf("ch3 should still receive after bob subscribed, got %+v", got)
	}
}

// channelClosed reports whether ch is closed, draining any buffered events
// first, within a short timeout.
func channelClosed(t *testing.T, ch <-chan Event) bool {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return true
			}
			// drain a buffered event and keep looking for the close
		case <-deadline:
			return false
		}
	}
}

func TestActiveCountTracksLifecycle(t *testing.T) {
	h := New(Config{})
	ctx := t.Context()
	hub := h.(*inProcHub)
	if got := hub.ActiveCount(); got != 0 {
		t.Errorf("initial active=%d, want 0", got)
	}

	_, u1, _ := h.Subscribe(ctx, SubscribeOpts{UserID: "alice", Topics: []string{"all"}})
	_, u2, _ := h.Subscribe(ctx, SubscribeOpts{UserID: "bob", Topics: []string{"all"}})
	if got := hub.ActiveCount(); got != 2 {
		t.Errorf("after 2 subs active=%d, want 2", got)
	}
	u1()
	if got := hub.ActiveCount(); got != 1 {
		t.Errorf("after 1 unsubscribe active=%d, want 1", got)
	}
	u2()
	if got := hub.ActiveCount(); got != 0 {
		t.Errorf("after both unsubscribed active=%d, want 0", got)
	}
}

func TestPublishWithoutSubscribersDoesNotPanic(t *testing.T) {
	h := New(Config{})
	for i := range 5 {
		if err := h.Publish(t.Context(), Event{Topic: "job:foo"}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
}

func TestNoGoroutineLeakOnSubscribeUnsubscribeCycle(t *testing.T) {
	// Hub itself spawns no goroutines; this guards against accidental
	// regressions if the implementation grows a per-subscriber watcher.
	h := New(Config{})
	ctx := t.Context()

	before := runtime.NumGoroutine()
	for range 100 {
		_, unsub, err := h.Subscribe(ctx, SubscribeOpts{UserID: "alice", Topics: []string{"all"}})
		if err != nil {
			t.Fatal(err)
		}
		unsub()
	}
	// Give the runtime a moment to recycle anything GC-aware.
	time.Sleep(20 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Errorf("goroutine leak: before=%d after=%d", before, after)
	}
}

func TestSubscriberReceivesTimestampedEvent(t *testing.T) {
	// Publish auto-fills CreatedAt when zero.
	h := New(Config{})
	ctx := t.Context()
	ch, unsub, _ := h.Subscribe(ctx, SubscribeOpts{UserID: "alice", Topics: []string{"all"}})
	defer unsub()
	before := time.Now().UTC()
	_ = h.Publish(ctx, Event{Topic: "job:foo", UserID: "alice"})
	after := time.Now().UTC().Add(time.Millisecond)

	got, _ := drain(t, ch, 1, time.Second)
	if len(got) != 1 {
		t.Fatal("expected one event")
	}
	if got[0].CreatedAt.Before(before) || got[0].CreatedAt.After(after) {
		t.Errorf("CreatedAt %v outside [%v, %v]", got[0].CreatedAt, before, after)
	}
}
