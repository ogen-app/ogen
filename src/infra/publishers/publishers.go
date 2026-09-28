// Package publishers defines the abstraction Ogen uses to surface
// "where can I publish posts?" information into the platforms API.
//
// One Publisher corresponds to one backend (today: Zernio; future:
// Buffer, Hootsuite, native Meta Graph, etc.). Each publisher knows
// which Ogen platforms it supports, which post types it can publish
// in for each, and which social accounts the user has connected.
//
// The handler queries every registered Publisher exactly once per
// request via PlatformViews so I/O scales with publisher count, not
// platform count. Publishers cache or batch internally; the contract
// here is just "give me everything you support".
package publishers

import (
	"context"
	"time"
)

// Publisher is the runtime view of one publishing backend. The platforms
// handler holds a slice of these and asks each for its full snapshot
// once per request.
type Publisher interface {
	// ID returns the stable slug used in API responses ("zernio").
	ID() string

	// Name returns the human label rendered in pickers ("Zernio").
	Name() string

	// State reports the integration's runtime health. Conventional
	// values mirror the Zernio integration: "disabled", "degraded",
	// "ok". Other publishers are free to use the same vocabulary.
	State() string

	// PlatformViews returns one PlatformView per Ogen platform this
	// publisher supports. Called exactly once per /api/platforms
	// request — do batched I/O here, not per-platform lookups.
	PlatformViews(ctx context.Context) ([]PlatformView, error)
}

// PlatformView is what one publisher reports for one Ogen platform.
//
// The handler matches each view to a local models.Platform row using
// OgenPlatformID first and PlatformName (case-insensitive) as a
// fallback. The fallback exists because the seeded platform IDs
// drift in some deployments (e.g. when a test suite has been run
// against a dev DB and replaced the seeded "linkedin" rows with
// fresh Sqid IDs). Publishers should populate both fields whenever
// possible — ID is faster, name is more durable.
//
// PublisherPlatformID is the wire identifier this publisher uses for
// the platform — distinct from OgenPlatformID for backends whose
// vocabulary differs from Ogen's (e.g. Zernio's "twitter" vs Ogen's
// "x-twitter"). The platforms handler joins it against the
// auto-publish allowlist to populate auto_publish_allowed.
//
// SupportedPostTypes references keys in the matched platform's
// PostTypes map. Keys the platform doesn't define are tolerated by
// the response but indicate either a stale allowlist or a custom
// platform the publisher can't fully describe.
type PlatformView struct {
	OgenPlatformID      string
	PlatformName        string
	PublisherPlatformID string
	SupportedPostTypes  []string
	Accounts            []Account
}

// Account is the minimal account projection the platforms response
// surfaces. Full detail (last_synced_at, raw_json) stays on the
// publisher's own /accounts endpoint.
type Account struct {
	ID          string
	Username    string
	DisplayName string
	AvatarURL   string
	IsActive    bool
	ConnectedAt time.Time
}
