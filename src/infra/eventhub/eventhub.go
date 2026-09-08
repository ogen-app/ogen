// Package eventhub implements an in-process pub/sub broker that decouples
// event producers (any backend code) from event consumers (long-lived SSE
// connections, internal listeners, etc.).
//
// The Hub interface is intentionally minimal:
//
//	Publish(ctx, Event) error
//	Subscribe(ctx, SubscribeOpts) (<-chan Event, func(), error)
//
// so a future out-of-process implementation (Redis / NATS / Postgres
// LISTEN/NOTIFY) can be dropped in without touching publishers or
// subscribers. Publishers never know HTTP exists; subscribers never know
// where events came from.
//
// Two filters run between Publish and Subscribe:
//
//   - Topic match — subscribers declare topic patterns at subscribe time
//     ("entity:post:abc", "job:*", or the catch-all "all"). See topic.go.
//   - Authorization — server-side check that the subscriber's UserID
//     matches the event's UserID. Events with empty UserID are public
//     (visible to every subscriber whose topic filter matches).
//
// Backpressure is "disconnect, not drop": when a subscriber's bounded
// channel buffer is full, that subscriber is removed from the Hub and
// its channel closed. The SSE handler interprets a closed channel as
// "you've been disconnected, exit"; the client then reconnects and
// reconciles state via REST. This keeps a slow consumer from blocking
// the Hub or other subscribers.
package eventhub

import (
	"context"
	"errors"
	"time"
)

// Event is the unit of communication on the Hub. Publishers populate it;
// subscribers receive it through their channel.
type Event struct {
	// ID is a globally unique identifier for the event (ULID-like string).
	// Forward-compat hook for `Last-Event-ID` replay if a future transport
	// supports it; ignored at the in-process layer.
	ID string `json:"id"`

	// Topic is the routing key. Subscribers filter by topic glob.
	// Conventional shapes: "entity:<kind>:<id>", "job:<id>", "user:<id>".
	Topic string `json:"topic"`

	// Type is a short hint about what changed — "updated", "completed",
	// "failed", etc. The UI uses it as a discriminator.
	Type string `json:"type"`

	// Payload is JSON-marshallable data; the SSE handler emits it as the
	// `data:` line.
	Payload any `json:"payload,omitempty"`

	// CreatedAt is set by Publish if zero.
	CreatedAt time.Time `json:"created_at"`

	// UserID drives the per-user authorization filter. Empty = not
	// user-scoped; non-empty = only subscribers with the matching UserID
	// receive this event.
	UserID string `json:"-"`

	// TenantID is the hard tenant-isolation boundary (CON-97 §10.2). When set,
	// only subscribers in the same tenant can receive the event. Publish
	// derives it from the context's tenant when left empty, so most publishers
	// need not set it explicitly.
	TenantID string `json:"-"`
}

// SubscribeOpts controls what a subscriber sees and how much they can
// buffer before backpressure kicks in.
type SubscribeOpts struct {
	// UserID identifies the subscriber for authorization. The Hub trusts
	// this field — callers (HTTP handlers) must populate it from a
	// verified source like a session, never from query params or headers.
	UserID string

	// TenantID scopes the subscriber to its tenant (CON-97 §10.2). The HTTP
	// handler sets it from the session; a subscriber only ever receives events
	// carrying the same TenantID.
	TenantID string

	// Topics is the list of glob patterns the subscriber wants to receive.
	// Must be non-empty. Use "all" to subscribe to every event.
	Topics []string

	// BufferSize overrides the Hub's default per-subscriber channel size.
	// 0 = use Config.BufferSize.
	BufferSize int
}

// Hub is the broker contract. The package ships one in-process
// implementation; future transports (Redis / NATS / Postgres
// LISTEN/NOTIFY) can satisfy the same interface with no caller changes.
type Hub interface {
	// Publish fans the event out to every subscriber whose topic filter
	// matches and whose UserID passes the authorization check. Never
	// blocks: subscribers whose buffer is full are disconnected.
	Publish(ctx context.Context, ev Event) error

	// Subscribe registers a new subscriber and returns its event channel
	// plus an unsubscribe function. The caller MUST defer the unsubscribe
	// — the Hub does not auto-clean up on ctx cancel. The channel is
	// closed when either the caller unsubscribes or the Hub disconnects
	// the subscriber due to backpressure.
	Subscribe(ctx context.Context, opts SubscribeOpts) (<-chan Event, func(), error)
}

// Sentinel errors returned from Subscribe. Callers can check via errors.Is.
var (
	// ErrNoTopics is returned when SubscribeOpts.Topics is empty.
	ErrNoTopics = errors.New("eventhub: at least one topic is required")

	// ErrTooManySubscribers signals that a user is at Config.MaxSubscribersPerUser.
	// The in-process Hub no longer returns it — at the cap it evicts the user's
	// oldest subscriber and admits the newcomer (CON-286), so a reload can never
	// be locked out. The sentinel and the HTTP handlers' 429 mapping are retained
	// for a future out-of-process backend that may not be able to evict cheaply
	// and would reject instead.
	ErrTooManySubscribers = errors.New("eventhub: subscriber limit exceeded for user")
)

// Config controls Hub-wide defaults. Pass to New; zero values fall back
// to sensible defaults so callers can pass Config{}.
type Config struct {
	// BufferSize is the default per-subscriber channel buffer size.
	// 0 = 128.
	BufferSize int

	// MaxSubscribersPerUser caps how many concurrent subscriptions a
	// single UserID can hold. Prevents one client from opening hundreds
	// of SSE connections. 0 = 10.
	MaxSubscribersPerUser int
}

func (c *Config) defaulted() Config {
	out := *c
	if out.BufferSize <= 0 {
		out.BufferSize = 128
	}
	if out.MaxSubscribersPerUser <= 0 {
		out.MaxSubscribersPerUser = 10
	}
	return out
}
