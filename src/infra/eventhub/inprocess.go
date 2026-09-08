package eventhub

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// New returns the in-process Hub implementation. Safe for concurrent use
// by any number of publishers and subscribers.
func New(cfg Config) Hub {
	return &inProcHub{
		cfg:  cfg.defaulted(),
		subs: make(map[uint64]*subscriber),
	}
}

type subscriber struct {
	id       uint64
	userID   string
	tenantID string
	topics   []string
	ch       chan Event
}

// matches returns true when the subscriber should receive ev. Combines the
// hard tenant boundary (CON-97 §10.2) with per-user authz and topic glob match.
func (s *subscriber) matches(ev Event) bool {
	// A tenant-tagged event never crosses into another tenant. Events carry a
	// tenant whenever they are published from a tenant context (Publish derives
	// it) or a publisher sets it; user-scoped events that predate a tenant are
	// still isolated by the UserID check below.
	if ev.TenantID != "" && ev.TenantID != s.tenantID {
		return false
	}
	if ev.UserID != "" && ev.UserID != s.userID {
		return false
	}
	return MatchAny(s.topics, ev.Topic)
}

type inProcHub struct {
	cfg Config

	mu     sync.RWMutex
	subs   map[uint64]*subscriber
	nextID uint64 // monotonic; protected by mu
	// active stays in sync with len(subs) but is exposed atomically for
	// observability/log lines without taking the mutex.
	active atomic.Int64
}

// Publish iterates the subscriber map under RLock, attempting a
// non-blocking send to each match. Subscribers whose buffers are full
// are collected and disconnected after RUnlock.
func (h *inProcHub) Publish(ctx context.Context, ev Event) error {
	// Stamp the tenant from the publishing context so most publishers don't
	// have to set it explicitly (CON-97 §10.2). An explicit ev.TenantID wins.
	if ev.TenantID == "" {
		if tid, ok := tenantctx.From(ctx); ok {
			ev.TenantID = tid
		}
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now().UTC()
	}

	h.mu.RLock()
	var toRemove []uint64
	for id, sub := range h.subs {
		if !sub.matches(ev) {
			continue
		}
		select {
		case sub.ch <- ev:
			// delivered
		default:
			// backpressure — disconnect this subscriber after RUnlock.
			toRemove = append(toRemove, id)
		}
	}
	h.mu.RUnlock()

	if len(toRemove) > 0 {
		h.disconnect(toRemove, "backpressure")
	}
	return nil
}

// disconnect removes the given subscriber IDs from the map and closes
// their channels. Idempotent — IDs already removed are silently skipped.
// Safe to call concurrently; the per-ID delete+close is mutex-serialized.
func (h *inProcHub) disconnect(ids []uint64, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range ids {
		h.removeLocked(id, reason)
	}
}

// removeLocked deletes one subscriber and closes its channel. The caller
// must hold h.mu. Closing the channel is also what unblocks a subscriber's
// reader: an SSE writer goroutine parked in `select { case <-eventCh }` wakes
// with a closed channel, returns, and runs its deferred unsubscribe — so this
// is how the Hub reclaims a slot whose writer would otherwise never exit
// (CON-286). Idempotent: an already-removed id is skipped.
func (h *inProcHub) removeLocked(id uint64, reason string) {
	sub, ok := h.subs[id]
	if !ok {
		return
	}
	delete(h.subs, id)
	close(sub.ch)
	h.active.Add(-1)
	slog.Info("subscriber disconnected", logging.AttrComponent, "eventhub", "id", sub.id, "user", sub.userID, "reason", reason, "topics", sub.topics)
}

// userCountLocked returns how many subscribers the user currently holds.
// The caller must hold h.mu.
func (h *inProcHub) userCountLocked(userID string) int {
	count := 0
	for _, s := range h.subs {
		if s.userID == userID {
			count++
		}
	}
	return count
}

// oldestForUserLocked returns the id of the user's longest-lived subscriber
// (the smallest id — nextID is monotonic, so lower means earlier). ok is false
// when the user has none. The caller must hold h.mu.
func (h *inProcHub) oldestForUserLocked(userID string) (id uint64, ok bool) {
	for sid, s := range h.subs {
		if s.userID != userID {
			continue
		}
		if !ok || sid < id {
			id, ok = sid, true
		}
	}
	return id, ok
}

// Subscribe registers a new subscriber and returns its read channel plus
// an unsubscribe function. The unsubscribe must be called (typically via
// defer) — the Hub does not auto-clean up on ctx cancel.
func (h *inProcHub) Subscribe(_ context.Context, opts SubscribeOpts) (<-chan Event, func(), error) {
	if len(opts.Topics) == 0 {
		return nil, nil, ErrNoTopics
	}
	bufferSize := opts.BufferSize
	if bufferSize <= 0 {
		bufferSize = h.cfg.BufferSize
	}

	h.mu.Lock()
	// Per-user subscription cap. At the cap we evict the user's OLDEST
	// subscriber (lowest id, since ids are monotonic) rather than reject the
	// incoming one (CON-286). Rejecting the newest is the wrong failure mode:
	// an SSE slot leaks whenever a writer goroutine can't observe its client
	// going away (a vanished peer whose tiny heartbeat writes keep buffering
	// never surfaces an error), and once the cap is full of such zombies every
	// new connection — a page reload, a fresh tab — is refused *permanently*
	// for that user until the process restarts. Evicting oldest-first makes the
	// cap self-healing: a reload always succeeds, closing the evicted channel
	// unblocks that zombie's parked goroutine so it finally unsubscribes, and a
	// genuine leak degrades to bounded wasted memory instead of a total outage.
	for h.userCountLocked(opts.UserID) >= h.cfg.MaxSubscribersPerUser {
		oldest, ok := h.oldestForUserLocked(opts.UserID)
		if !ok {
			break // no evictable subscriber (cap effectively 0); admit anyway
		}
		h.removeLocked(oldest, "evicted: subscriber limit reached")
	}
	h.nextID++
	id := h.nextID
	sub := &subscriber{
		id:       id,
		userID:   opts.UserID,
		tenantID: opts.TenantID,
		topics:   append([]string(nil), opts.Topics...), // defensive copy
		ch:       make(chan Event, bufferSize),
	}
	h.subs[id] = sub
	h.active.Add(1)
	h.mu.Unlock()

	slog.Info("subscriber connected", logging.AttrComponent, "eventhub", "id", sub.id, "user", sub.userID, "topics", sub.topics, "buffer", bufferSize)

	unsubscribe := func() {
		h.disconnect([]uint64{id}, "unsubscribed")
	}
	return sub.ch, unsubscribe, nil
}

// ActiveCount returns the number of currently-connected subscribers.
// Useful for tests and an eventual /health check; not part of the Hub
// interface so it doesn't leak into a future out-of-process backend.
func (h *inProcHub) ActiveCount() int64 {
	return h.active.Load()
}
