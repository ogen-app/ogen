package entitlements

import (
	"context"
	"expvar"
	"fmt"
	"log/slog"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

// Mode controls how the Limiter reacts to an over-cap create or a closed gate
// (CON-295). warn-first is the shipping default; flip to enforce via config.
type Mode string

const (
	ModeEnforce Mode = "enforce" // block: over-cap creates return a quota error
	ModeWarn    Mode = "warn"    // allow, but log + count the would-block
	ModeOff     Mode = "off"     // no-op: limits are not consulted
)

// ParseMode maps a config string to a Mode, defaulting to warn for empty or
// unrecognised input.
func ParseMode(s string) Mode {
	switch Mode(s) {
	case ModeEnforce, ModeOff:
		return Mode(s)
	default:
		return ModeWarn
	}
}

// wouldBlock counts denials that were allowed through only because the mode is
// warn — the signal to watch before flipping to enforce.
var wouldBlock = expvar.NewInt("entitlement_would_blocks")

// Counter reports a tenant's current live usage for one numeric feature. Counts
// come from the control-plane DB so enforcement never fails open on the analytics
// pool (CON-295 decision 1).
type Counter interface {
	Count(ctx context.Context, tenantID string) (int64, error)
}

// CounterFunc adapts a plain function to a Counter.
type CounterFunc func(ctx context.Context, tenantID string) (int64, error)

// Count implements Counter.
func (f CounterFunc) Count(ctx context.Context, tenantID string) (int64, error) {
	return f(ctx, tenantID)
}

// Decision is the outcome of a numeric quota check.
type Decision struct {
	Key     string
	Allowed bool
	Limit   *int64 // nil = unlimited
	Current int64
}

// Err returns a *QuotaExceededError when the decision denied the action, else nil.
func (d Decision) Err() error {
	if d.Allowed {
		return nil
	}
	var limit int64
	if d.Limit != nil {
		limit = *d.Limit
	}
	return &QuotaExceededError{Key: d.Key, Limit: limit, Current: d.Current}
}

// QuotaExceededError is returned when a numeric entitlement cap is reached; the
// transport layer renders it as HTTP 402 entitlement_exceeded.
type QuotaExceededError struct {
	Key     string
	Limit   int64
	Current int64
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("entitlement_exceeded: %s (limit %d, current %d)", e.Key, e.Limit, e.Current)
}

// FeatureNotAvailableError is returned when a boolean capability is off; the
// transport layer renders it as HTTP 403 feature_not_available.
type FeatureNotAvailableError struct{ Key string }

func (e *FeatureNotAvailableError) Error() string { return "feature_not_available: " + e.Key }

// currentResolver is the slice of the resolver the Limiter needs (so tests can
// fake it). *Resolver satisfies it.
type currentResolver interface {
	ResolveCurrent(ctx context.Context, tenantID string) (*Resolution, error)
}

// Limiter enforces per-tenant entitlement quotas at resource-creation time
// (CON-295). Numeric features are capped via registered Counters; boolean
// features are gates; multiplier features scale a downstream budget. All methods
// are nil-safe so a handler can call them whether or not a Limiter is wired.
type Limiter struct {
	resolver currentResolver
	catalog  *Catalog
	counters map[string]Counter
	mode     Mode
}

// NewLimiter builds a Limiter over the resolver + catalog in the given mode.
func NewLimiter(resolver currentResolver, catalog *Catalog, mode Mode) *Limiter {
	return &Limiter{resolver: resolver, catalog: catalog, counters: map[string]Counter{}, mode: mode}
}

// Register attaches a Counter to a numeric feature key (wiring time).
func (l *Limiter) Register(featureKey string, c Counter) *Limiter {
	l.counters[featureKey] = c
	return l
}

// Require checks a numeric cap and returns a *QuotaExceededError when the tenant
// is at/over the cap in enforce mode (nil otherwise). The one-liner handlers use.
func (l *Limiter) Require(ctx context.Context, tenantID, featureKey string) error {
	if l == nil {
		return nil
	}
	dec, err := l.Allow(ctx, tenantID, featureKey)
	if err != nil {
		return err
	}
	return dec.Err()
}

// RequireGate checks a boolean capability and returns a *FeatureNotAvailableError
// when it is off in enforce mode (nil otherwise). Callers apply their own
// contextual precondition before calling (e.g. only for a non-Evergreen type).
func (l *Limiter) RequireGate(ctx context.Context, tenantID, featureKey string) error {
	if l == nil {
		return nil
	}
	ok, err := l.Gate(ctx, tenantID, featureKey)
	if err != nil {
		return err
	}
	if !ok {
		return &FeatureNotAvailableError{Key: featureKey}
	}
	return nil
}

// Allow reports whether the tenant may create one more of the numeric resource
// behind featureKey. Unlimited (nil) always allows; otherwise Allowed = current
// < limit. warn/off never block (warn logs + counts the would-block). Failures
// to resolve or count fail OPEN so a hiccup never wedges creates.
func (l *Limiter) Allow(ctx context.Context, tenantID, featureKey string) (Decision, error) {
	dec := Decision{Key: featureKey, Allowed: true}
	if l == nil || l.mode == ModeOff {
		return dec, nil
	}
	limit, present, err := l.numericValue(ctx, tenantID, featureKey)
	if err != nil {
		l.failOpen(ctx, featureKey, "resolve", err)
		return dec, nil
	}
	if !present || limit == nil {
		return dec, nil // unlimited, or the feature is not on this version
	}
	dec.Limit = limit
	counter, has := l.counters[featureKey]
	if !has {
		slog.WarnContext(ctx, "entitlements: no counter registered; allowing", logging.AttrComponent, "entitlements", "feature", featureKey)
		return dec, nil
	}
	current, err := counter.Count(ctx, tenantID)
	if err != nil {
		l.failOpen(ctx, featureKey, "count", err)
		return dec, nil
	}
	dec.Current = current
	if current < *limit {
		return dec, nil
	}
	if l.mode == ModeEnforce {
		dec.Allowed = false
		return dec, nil
	}
	// warn
	wouldBlock.Add(1)
	slog.WarnContext(ctx, "entitlements: cap reached (warn mode, allowed)", logging.AttrComponent, "entitlements", "feature", featureKey, "limit", *limit, "current", current)
	return dec, nil
}

// Gate reports whether a boolean capability is enabled. A missing feature or a
// resolve error is treated as enabled (fail open). warn/off never deny.
func (l *Limiter) Gate(ctx context.Context, tenantID, featureKey string) (bool, error) {
	if l == nil || l.mode == ModeOff {
		return true, nil
	}
	val, present, err := l.boolValue(ctx, tenantID, featureKey)
	if err != nil {
		l.failOpen(ctx, featureKey, "resolve", err)
		return true, nil
	}
	if !present || val {
		return true, nil
	}
	if l.mode == ModeEnforce {
		return false, nil
	}
	wouldBlock.Add(1)
	slog.WarnContext(ctx, "entitlements: gate closed (warn mode, allowed)", logging.AttrComponent, "entitlements", "feature", featureKey)
	return true, nil
}

// Multiplier returns the numeric multiplier entitlement for featureKey (e.g.
// assistant_multiplier), defaulting to 1 when unset, unlimited, or on any error.
func (l *Limiter) Multiplier(ctx context.Context, tenantID, featureKey string) float64 {
	if l == nil {
		return 1
	}
	limit, present, err := l.numericValue(ctx, tenantID, featureKey)
	if err != nil || !present || limit == nil || *limit <= 0 {
		return 1
	}
	return float64(*limit)
}

// numericValue returns (limit, present, err): limit is nil for an unlimited
// numeric entitlement. A non-numeric or absent value reports present=false.
func (l *Limiter) numericValue(ctx context.Context, tenantID, key string) (*int64, bool, error) {
	res, err := l.resolver.ResolveCurrent(ctx, tenantID)
	if err != nil {
		return nil, false, err
	}
	for i := range res.Entitlements {
		e := res.Entitlements[i]
		if e.Key != key {
			continue
		}
		if e.Value == nil {
			return nil, true, nil // unlimited
		}
		if f, ok := e.Value.(float64); ok {
			n := int64(f)
			return &n, true, nil
		}
		return nil, false, nil
	}
	return nil, false, nil
}

// boolValue returns (value, present, err) for a boolean entitlement.
func (l *Limiter) boolValue(ctx context.Context, tenantID, key string) (bool, bool, error) {
	res, err := l.resolver.ResolveCurrent(ctx, tenantID)
	if err != nil {
		return false, false, err
	}
	for i := range res.Entitlements {
		e := res.Entitlements[i]
		if e.Key != key {
			continue
		}
		if b, ok := e.Value.(bool); ok {
			return b, true, nil
		}
		return false, false, nil
	}
	return false, false, nil
}

func (l *Limiter) failOpen(ctx context.Context, featureKey, op string, err error) {
	slog.WarnContext(ctx, "entitlements: "+op+" failed; allowing", logging.AttrComponent, "entitlements", "feature", featureKey, logging.AttrError, err)
}
