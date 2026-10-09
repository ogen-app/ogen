package zernio

import (
	"sync"
	"time"
)

// State reflects whether the Zernio integration is usable.
//
//   - StateDisabled — no API key configured, or auth failed at boot
//     (401). Endpoints respond 409 integration_disabled. Worker is not
//     started. Only restart or admin /profile/repair can transition out.
//   - StateDegraded — API key valid but profile bootstrap has not yet
//     succeeded (transient network/5xx). Endpoints respond 503
//     integration_degraded. Worker keeps retrying.
//   - StateOK       — fully operational; all endpoints serve normally.
type State string

const (
	StateDisabled State = "disabled"
	StateDegraded State = "degraded"
	StateOK       State = "ok"
)

// Integration is the shared controller passed to handlers and the
// background sync worker. Exactly one *Integration exists per Ogen
// process; its mutex serialises state transitions across goroutines.
//
// A non-nil Integration with Client == nil represents an instance that
// was constructed without an API key — Enabled() returns false and the
// state is permanently StateDisabled.
type Integration struct {
	Client *Client

	mu    sync.RWMutex
	state State
	// fastUntil is each tenant's fast-polling deadline; the worker syncs those
	// tenants on the fast cadence until it passes.
	fastUntil map[string]time.Time
}

// NewIntegration wires a fresh controller around c. The initial state is
// always StateDisabled; bootstrap (Phase 2) transitions to degraded or
// ok depending on the outcome of the boot-time validation.
func NewIntegration(c *Client) *Integration {
	return &Integration{Client: c, state: StateDisabled}
}

// State returns the current integration state.
func (i *Integration) State() State {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.state
}

// SetState transitions the integration to s. Callers should log the
// transition rather than relying on this method to do it — log site
// context (which subsystem caused the transition) is more useful than
// a single shared log line here.
func (i *Integration) SetState(s State) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.state = s
}

// PromoteOK transitions a StateDegraded integration to StateOK atomically,
// returning true when it made the change. It is a no-op (returning false) from
// any other state, so a successful background sync can clear a *stale* degraded
// without ever resurrecting a disabled integration (no key / 401-rejected) or
// racing a concurrent disable. The compare-and-set is held under the same mutex
// as SetState so the read and write can't interleave with the worker's own 401
// demotion.
//
// This is the self-heal path. The boot/key-change warmup Ping is the only other
// writer of StateOK, and it never re-runs unless zernio_api_key changes — so a
// transient boot-time Ping failure (network blip / 5xx) would otherwise pin the
// instance to degraded for the life of the process, even while the key and every
// per-tenant profile work fine. A clean sync tick is positive proof to the
// contrary, so it promotes back to ok.
func (i *Integration) PromoteOK() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.state == StateDegraded {
		i.state = StateOK
		return true
	}
	return false
}

// Enabled reports whether the integration is currently usable: a client is
// wired AND it is not in the disabled state (no API key configured, or auth
// rejected). It is evaluated live off the current state, so setting, rotating,
// or clearing the zernio_api_key via the secrets API flips it without a reboot
// (the secrets subscription in initZernio re-validates and updates State).
// Endpoints short-circuit with 409 integration_disabled when this is false.
func (i *Integration) Enabled() bool {
	return i != nil && i.Client != nil && i.State() != StateDisabled
}

// BumpFastUntil extends tenantID's fast-polling deadline so the background
// sync worker syncs that tenant on the fast cadence after a connect link is
// issued; other tenants keep the regular one. Bumping past the existing
// deadline wins; bumps to an earlier time are ignored. Safe to call from any
// goroutine.
func (i *Integration) BumpFastUntil(tenantID string, deadline time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.fastUntil == nil {
		i.fastUntil = map[string]time.Time{}
	}
	if deadline.After(i.fastUntil[tenantID]) {
		i.fastUntil[tenantID] = deadline
	}
}

// FastTenants returns the tenants whose fast-polling window is open at now,
// dropping the ones whose window has closed.
func (i *Integration) FastTenants(now time.Time) []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	var out []string
	for tid, until := range i.fastUntil {
		if now.Before(until) {
			out = append(out, tid)
		} else {
			delete(i.fastUntil, tid)
		}
	}
	return out
}
