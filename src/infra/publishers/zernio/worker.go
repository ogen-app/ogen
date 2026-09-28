package zernio

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// SyncIntervalFloor is the hard lower bound the worker enforces in
// code regardless of the configured ZERNIO_SYNC_INTERVAL — a runaway
// env value can't melt Zernio's API.
const SyncIntervalFloor = 10 * time.Second

// fastSyncIntervalFloor is the hard lower bound for the adaptive fast
// cadence that activates after a connect-link issuance.
const fastSyncIntervalFloor = 5 * time.Second

// rateLimitBackoffCap caps the doubled interval after a 429 from
// Zernio (per the ticket: "double the next interval, capped at 5 min").
const rateLimitBackoffCap = 5 * time.Minute

// SyncStatusOK / SyncStatusError are the documented values written
// to the zernio.last_sync_status setting.
const (
	SyncStatusOK    = "ok"
	syncErrorPrefix = "error: "
)

// EventTopicSync is the event topic for sync-wide events (success /
// failure). Per-account events use a per-id topic — see eventTopicForAccount.
const EventTopicSync = "zernio:sync"

// EventTypeAttached / EventTypeAttachFailed / EventTypeUpdated /
// EventTypeDisconnected / EventTypeRevived are the eventhub event
// types published per account. The names align with ReconcileChange
// values plus the explicit "attach_failed" signal the ticket asks
// for on persistence failure.
const (
	EventTypeAttached     = "zernio.account.attached"
	EventTypeAttachFailed = "zernio.account.attach_failed"
	EventTypeUpdated      = "zernio.account.updated"
	EventTypeDisconnected = "zernio.account.disconnected"
	EventTypeRevived      = "zernio.account.revived"
	EventTypeSyncOK       = "zernio.sync.ok"
	EventTypeSyncFailed   = "zernio.sync.failed"
)

// Worker periodically reconciles Zernio's view of accounts against
// the local social_accounts table. Started by the host once at boot
// and stopped via Stop(). The loop:
//
//  1. Wait until the integration is in StateOK and a profile_id is
//     known (bootstrap may still be in flight).
//  2. Tick: list remote → list local → reconcile → ApplyPlan →
//     publish per-change events → write last_sync_at / last_sync_status.
//  3. Compute next interval honouring (a) ZernioSyncInterval, (b)
//     the fast-cadence window from BumpFastUntil, (c) rate-limit
//     backoff, (d) the SyncIntervalFloor hard cap.
//
// Errors are non-fatal except 401 from Zernio, which transitions the
// integration to StateDisabled and exits the loop until restart or an
// admin /profile/repair.
type Worker struct {
	integ        *Integration
	accounts     repository.SocialAccountRepository
	settings     SettingsStore
	hub          eventhub.Hub
	bootstrapper *Bootstrapper
	recorder     *usage.Recorder // account_connect events; nil = no-op

	// tenantsWithProfile lists the tenant ids that have a Zernio profile
	// configured; the sweep runs one tick per returned tenant. It is
	// invoked under a system context so it can read across tenants.
	tenantsWithProfile func(context.Context) ([]string, error)

	interval     time.Duration
	fastInterval time.Duration

	trigger chan struct{}
	done    chan struct{}

	// rateLimitUntil is set by tick() when Zernio returns 429; the
	// next iteration sleeps until at least this instant.
	rateLimitUntil time.Time
}

func NewWorker(
	integ *Integration,
	accounts repository.SocialAccountRepository,
	settings SettingsStore,
	hub eventhub.Hub,
	bootstrapper *Bootstrapper,
	recorder *usage.Recorder,
	interval, fastInterval time.Duration,
	tenantsWithProfile func(context.Context) ([]string, error),
) *Worker {
	if interval < SyncIntervalFloor {
		interval = SyncIntervalFloor
	}
	if fastInterval < fastSyncIntervalFloor {
		fastInterval = fastSyncIntervalFloor
	}
	return &Worker{
		integ:              integ,
		accounts:           accounts,
		settings:           settings,
		hub:                hub,
		bootstrapper:       bootstrapper,
		recorder:           recorder,
		tenantsWithProfile: tenantsWithProfile,
		interval:           interval,
		fastInterval:       fastInterval,
		trigger:            make(chan struct{}, 1),
		done:               make(chan struct{}),
	}
}

// Run blocks until ctx is cancelled, ticking on the configured
// cadence. Safe to call from a goroutine.
func (w *Worker) Run(ctx context.Context) {
	defer close(w.done)
	slog.InfoContext(ctx, "sync worker started",
		logging.AttrComponent, "zernio.worker",
		"interval", w.interval,
		"fast_interval", w.fastInterval)
	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "sync worker stopped",
				logging.AttrComponent, "zernio.worker")
			return
		default:
		}

		if !w.shouldTick() {
			// Bootstrap not done yet, or integration disabled — wait
			// out one full interval before checking again.
			if !w.sleep(ctx, w.interval) {
				return
			}
			continue
		}

		if err := w.syncAllTenants(ctx); err != nil {
			if IsStatus(err, http.StatusUnauthorized) {
				slog.WarnContext(ctx, "401 from zernio; disabling integration",
					logging.AttrComponent, "zernio.worker")
				w.integ.SetState(StateDisabled)
				return
			}
			slog.ErrorContext(ctx, "sync sweep failed",
				logging.AttrComponent, "zernio.worker",
				logging.AttrError, err)
		}

		if !w.sleep(ctx, w.nextInterval()) {
			return
		}
	}
}

// Done returns a channel closed when Run returns. Used by the host's
// shutdown hook to wait up to 2s for a graceful exit.
func (w *Worker) Done() <-chan struct{} { return w.done }

// TriggerNow asks the worker to run a tick at its earliest opportunity.
// Multiple calls coalesce — the channel is size 1 and a non-blocking
// send is dropped when one is already pending.
func (w *Worker) TriggerNow() {
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

// shouldTick reports whether a tick is worth attempting now. Avoids
// spamming Zernio when bootstrap hasn't populated a profile ID yet.
func (w *Worker) shouldTick() bool {
	if !w.integ.Enabled() || w.integ.State() == StateDisabled {
		return false
	}
	if time.Now().Before(w.rateLimitUntil) {
		return false
	}
	// Per-tenant profile presence is decided per sweep by tenantsWithProfile;
	// the sweep simply no-ops when no tenant has a profile.
	return true
}

// syncAllTenants enumerates the tenants that have a Zernio profile and runs one
// reconciliation tick per tenant, scoped to it. The enumeration is
// cross-tenant (system context); each tick runs under tenantctx.With so account
// upserts, last_sync_* settings, and SSE events land in the right tenant. A 401
// propagates (the shared key is bad → disable instance-wide); a 429 sets the
// shared rate-limit backoff inside tick and stops the sweep early.
func (w *Worker) syncAllTenants(ctx context.Context) error {
	tenants, err := w.tenantsWithProfile(tenantctx.WithSystem(ctx))
	if err != nil {
		return fmt.Errorf("list tenants with zernio profile: %w", err)
	}
	for _, tid := range tenants {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if time.Now().Before(w.rateLimitUntil) {
			break // shared rate limit hit earlier this sweep — retry next interval
		}
		if err := w.tick(tenantctx.With(ctx, tid)); err != nil {
			if IsStatus(err, http.StatusUnauthorized) {
				return err // bad shared key — disable instance-wide
			}
			slog.ErrorContext(ctx, "sync tenant failed",
				logging.AttrComponent, "zernio.worker",
				"tenant_id", tid,
				logging.AttrError, err)
		}
	}
	return nil
}

// SyncOnce runs a single per-tenant sync sweep synchronously and returns. Used
// by tests and available for a synchronous trigger path.
func (w *Worker) SyncOnce(ctx context.Context) error {
	return w.syncAllTenants(ctx)
}

// nextInterval returns the delay before the next tick, honouring fast
// cadence and any active rate-limit backoff. The floor still applies.
func (w *Worker) nextInterval() time.Duration {
	now := time.Now()
	if rl := w.rateLimitUntil; rl.After(now) {
		return rl.Sub(now)
	}
	d := w.interval
	if now.Before(w.integ.FastUntil()) {
		d = w.fastInterval
	}
	if d < SyncIntervalFloor {
		// Fast interval is allowed below SyncIntervalFloor (it has
		// its own 5s floor enforced at construction); only the
		// regular interval is bounded by SyncIntervalFloor.
		if d < fastSyncIntervalFloor {
			d = fastSyncIntervalFloor
		}
	}
	return d
}

// sleep blocks for d, returning false when ctx fires (so the caller
// can exit promptly) or when triggered (which short-circuits the wait).
func (w *Worker) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case <-w.trigger:
		return true
	}
}

// tick executes one reconciliation pass. Returns the underlying error
// so the loop can react to 401 vs 429 vs everything else.
//
// Log lines use the documented zernio.* key=value fields so a future
// structured-logging migration can ingest them without a regex pass.
func (w *Worker) tick(ctx context.Context) error {
	tickStart := time.Now()

	profileID, _, err := w.settings.Get(ctx, SettingProfileID)
	if err != nil {
		w.recordSyncStatus(ctx, err)
		return err
	}
	if profileID == "" {
		return nil // this tenant has no profile (raced with deletion) — nothing to sync
	}

	remote, err := w.integ.Client.ListAccounts(ctx, profileID)
	if err != nil {
		if IsStatus(err, http.StatusTooManyRequests) {
			w.handleRateLimit()
		}
		w.recordSyncStatus(ctx, err)
		w.publishSyncEvent(ctx, EventTypeSyncFailed, fmt.Sprintf("list_accounts: %v", err))
		slog.ErrorContext(ctx, "sync failed",
			logging.AttrComponent, "zernio.worker",
			"profile_id", profileID,
			"sync_tick_ms", time.Since(tickStart).Milliseconds(),
			logging.AttrError, err)
		return err
	}

	// A successful authenticated list is positive proof the shared key and this
	// tenant's profile both work, so clear any stale app-wide StateDegraded (the
	// warmup Ping only re-runs on a key change, and the lazy connect-link
	// bootstrap only re-runs for a tenant with no profile — so without this a
	// transient boot-time Ping failure would strand the instance in degraded for
	// the life of the process). PromoteOK is a no-op unless degraded, so it never
	// resurrects a disabled (401) integration.
	if w.integ.PromoteOK() {
		slog.InfoContext(ctx, "sync recovered; integration promoted degraded to ok",
			logging.AttrComponent, "zernio.worker",
			"profile_id", profileID)
	}

	local, err := w.accounts.ListAll(ctx, profileID)
	if err != nil {
		w.recordSyncStatus(ctx, err)
		return err
	}

	now := time.Now().UTC()
	plan := Reconcile(remote, local, profileID, now)

	if err := w.accounts.ApplyPlan(ctx, plan.Upserts, plan.SoftDeleteIDs, now); err != nil {
		w.recordSyncStatus(ctx, err)
		for _, ch := range plan.Changes {
			if ch.Change == ChangeAttached {
				w.publishAccountEvent(ctx, EventTypeAttachFailed, ch.Account, fmt.Sprintf("apply_plan: %v", err))
			}
		}
		slog.ErrorContext(ctx, "sync failed",
			logging.AttrComponent, "zernio.worker",
			"profile_id", profileID,
			"sync_tick_ms", time.Since(tickStart).Milliseconds(),
			logging.AttrError, err)
		return err
	}

	var added, updated, removed int
	for _, ch := range plan.Changes {
		switch ch.Change {
		case ChangeAttached:
			added++
			w.publishAccountEvent(ctx, EventTypeAttached, ch.Account, "")
			w.recordAccountConnect(ctx, ch.Account)
			slog.InfoContext(ctx, "account attached",
				logging.AttrComponent, "zernio.worker",
				"profile_id", profileID,
				"platform", ch.Account.Platform,
				"account_id", ch.Account.ID)
		case ChangeUpdated:
			updated++
			w.publishAccountEvent(ctx, EventTypeUpdated, ch.Account, "")
		case ChangeDisconnected:
			removed++
			w.publishAccountEvent(ctx, EventTypeDisconnected, ch.Account, "")
			slog.InfoContext(ctx, "account disconnected",
				logging.AttrComponent, "zernio.worker",
				"profile_id", profileID,
				"platform", ch.Account.Platform,
				"account_id", ch.Account.ID)
		case ChangeRevived:
			added++
			w.publishAccountEvent(ctx, EventTypeRevived, ch.Account, "")
			w.recordAccountConnect(ctx, ch.Account)
		}
	}

	w.refreshProfileMeta(ctx, profileID)

	w.recordSyncStatus(ctx, nil)
	w.publishSyncEvent(ctx, EventTypeSyncOK, fmt.Sprintf("upserts=%d soft_deletes=%d", len(plan.Upserts), len(plan.SoftDeleteIDs)))
	slog.InfoContext(ctx, "sync ok",
		logging.AttrComponent, "zernio.worker",
		"profile_id", profileID,
		"sync_tick_ms", time.Since(tickStart).Milliseconds(),
		"accounts_added", added,
		"accounts_updated", updated,
		"accounts_removed", removed)
	return nil
}

// handleRateLimit doubles the next interval, capped at the documented 5m.
func (w *Worker) handleRateLimit() {
	delay := min(w.interval*2, rateLimitBackoffCap)
	w.rateLimitUntil = time.Now().Add(delay)
	slog.Warn("rate-limited; backing off",
		logging.AttrComponent, "zernio.worker",
		"backoff", delay)
}

// recordSyncStatus writes the documented last_sync_at / last_sync_status
// settings. Errors here are logged but not propagated — a status-write
// failure shouldn't suppress the underlying tick error.
func (w *Worker) recordSyncStatus(ctx context.Context, syncErr error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if err := w.settings.Set(ctx, SettingLastSyncAt, now); err != nil {
		slog.WarnContext(ctx, "write setting failed",
			logging.AttrComponent, "zernio.worker",
			"setting", SettingLastSyncAt,
			logging.AttrError, err)
	}
	status := SyncStatusOK
	if syncErr != nil {
		status = syncErrorPrefix + truncate(syncErr.Error(), 200)
	}
	if err := w.settings.Set(ctx, SettingLastSyncStatus, status); err != nil {
		slog.WarnContext(ctx, "write setting failed",
			logging.AttrComponent, "zernio.worker",
			"setting", SettingLastSyncStatus,
			logging.AttrError, err)
	}
}

// refreshProfileMeta re-reads the profile from Zernio and updates the
// settings cache. Best-effort: failure is logged and swallowed.
func (w *Worker) refreshProfileMeta(ctx context.Context, profileID string) {
	profile, err := w.integ.Client.GetProfile(ctx, profileID)
	if err != nil {
		slog.WarnContext(ctx, "refresh profile meta failed",
			logging.AttrComponent, "zernio.worker",
			logging.AttrError, err)
		return
	}
	raw := profile.Raw
	if len(raw) == 0 {
		var mErr error
		if raw, mErr = json.Marshal(profile); mErr != nil {
			slog.WarnContext(ctx, "marshal profile meta failed",
				logging.AttrComponent, "zernio.worker",
				logging.AttrError, mErr)
			return
		}
	}
	if err := w.settings.Set(ctx, SettingProfileMeta, string(raw)); err != nil {
		slog.WarnContext(ctx, "write setting failed",
			logging.AttrComponent, "zernio.worker",
			"setting", SettingProfileMeta,
			logging.AttrError, err)
	}
	if err := w.settings.Set(ctx, SettingProfileName, profile.Name); err != nil {
		slog.WarnContext(ctx, "write setting failed",
			logging.AttrComponent, "zernio.worker",
			"setting", SettingProfileName,
			logging.AttrError, err)
	}
}

// recordAccountConnect emits a CON-86 account_connect usage event when a social
// account is attached or revived — the "Zernio platform (social account) added"
// event. Count-only (the connect itself isn't billed) but tracked with the
// platform + social_account_id dimensions. The tick ctx is tenant-scoped; a nil
// recorder is a no-op.
func (w *Worker) recordAccountConnect(ctx context.Context, account models.SocialAccount) {
	w.recorder.Record(ctx, VendorZernio, "zernio_sync", vendors.MeterEvent{
		Model:           account.Platform,
		Operation:       OpAccountConnect,
		Usage:           vendors.Usage{vendors.KindAccount: 1},
		Platform:        account.Platform,
		SocialAccountID: account.ID,
	})
}

// PublishAccountDisconnected fires the account.disconnected eventhub event for
// a user-initiated disconnect, reusing the same topic + payload shape
// the reconciler emits when Zernio drops an account so subscribers can't tell
// the two apart. The ctx must be tenant-scoped (the hub derives TenantID from
// it). Best-effort: publish failures are logged, not returned.
func (w *Worker) PublishAccountDisconnected(ctx context.Context, account models.SocialAccount) {
	w.publishAccountEvent(ctx, EventTypeDisconnected, account, "")
}

// RecordAccountDisconnect emits a CON-133 account_disconnect usage event,
// mirroring recordAccountConnect: count-only, dimensioned by platform +
// social_account_id. The ctx must be tenant-scoped; a nil recorder is a no-op.
func (w *Worker) RecordAccountDisconnect(ctx context.Context, account models.SocialAccount) {
	w.recorder.Record(ctx, VendorZernio, "zernio_sync", vendors.MeterEvent{
		Model:           account.Platform,
		Operation:       OpAccountDisconnect,
		Usage:           vendors.Usage{vendors.KindAccount: 1},
		Platform:        account.Platform,
		SocialAccountID: account.ID,
	})
}

// publishAccountEvent fires an eventhub event for one account change.
// Topic shape matches the in-tree convention (entity:<kind>:<id>).
func (w *Worker) publishAccountEvent(ctx context.Context, eventType string, account models.SocialAccount, errMsg string) {
	topic := fmt.Sprintf("entity:zernio_account:%s", account.ID)
	payload := map[string]any{
		"id":          account.ID,
		"platform":    account.Platform,
		"username":    account.Username,
		"displayName": account.DisplayName,
		"isActive":    account.IsActive,
	}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	// Scope the event to the tenant being synced via the context, not the
	// account: newly attached/updated accounts are projected from the Zernio
	// API and carry no tenant_id yet (it's stamped on DB write). The hub
	// derives TenantID from the scoped ctx — same as publishSyncEvent — so
	// event scoping stays independent of repository stamping.
	if err := w.hub.Publish(ctx, eventhub.Event{
		Topic:   topic,
		Type:    eventType,
		Payload: payload,
	}); err != nil {
		slog.WarnContext(ctx, "publish event failed",
			logging.AttrComponent, "zernio.worker",
			"event_type", eventType,
			logging.AttrError, err)
	}
}

// publishSyncEvent fires the broad sync.ok / sync.failed signal.
func (w *Worker) publishSyncEvent(ctx context.Context, eventType, summary string) {
	if err := w.hub.Publish(ctx, eventhub.Event{
		Topic:   EventTopicSync,
		Type:    eventType,
		Payload: map[string]any{"summary": summary},
	}); err != nil {
		slog.WarnContext(ctx, "publish event failed",
			logging.AttrComponent, "zernio.worker",
			"event_type", eventType,
			logging.AttrError, err)
	}
}

// truncate caps s at n characters with an ellipsis when needed. Used
// to keep last_sync_status from blowing up the settings row.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
