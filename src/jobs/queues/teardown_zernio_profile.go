package queues

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// TeardownZernioProfileQueue tears down a tenant's Zernio profile when its
// workspace is deleted (CON-203, follow-up to CON-147 PR4). WorkspacesHandler.Delete
// enqueues one task inside its soft-delete transaction (transactional outbox),
// so the teardown exists iff the workspace was actually deleted and the delete
// HTTP response never blocks on Zernio. It is the mirror image of
// BootstrapZernioProfileQueue: bootstrap provisions the per-tenant profile at
// signup, teardown removes it at deletion.
//
// Deferring the actual Zernio calls into a retryable job is what keeps the
// delete request fast and resilient: Zernio being unreachable delays cleanup,
// it never fails the delete.
const TeardownZernioProfileQueue = "teardown_zernio_profile"

// TeardownZernioProfileTask carries the tenant whose profile to delete.
type TeardownZernioProfileTask struct {
	TenantID string `json:"tenant_id"`
}

// Kind implements river.JobArgs.
func (TeardownZernioProfileTask) Kind() string { return TeardownZernioProfileQueue }

// InsertOpts bounds retries. No UniqueOpts is needed: the delete handler
// enqueues exactly one task per tenant inside the soft-delete tx (tenant ids are
// unique and the insert commits with the deletion), and the work is idempotent —
// a re-run against an already-gone profile is a no-op.
func (TeardownZernioProfileTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 5}
}

// TeardownZernioProfileProcessor runs one profile-teardown attempt. It resolves
// the tenant's Zernio profile id from settings (written by the bootstrapper),
// disconnects every connected account, then deletes the profile.
type TeardownZernioProfileProcessor struct {
	river.WorkerDefaults[TeardownZernioProfileTask]
	Integration *zernio.Integration
	// Settings reads the tenant-scoped zernio.profile_id (and clears the profile
	// keys after a successful delete). Same store the bootstrapper writes.
	Settings zernio.SettingsStore
	// Tenants short-circuits teardown for a tenant restored between the
	// workspace-delete enqueue and this run (CON-190): never delete a live
	// tenant's profile. Fail-closed — a nil reader (or a status it can't
	// resolve) reads as "active", so teardown skips rather than risk deleting a
	// profile it can't confirm is dead. In prod it is always wired (r.tenantRepo).
	Tenants TenantStatusReader
}

// Work is the River entrypoint. Like bootstrap, this job is scoped to a single
// tenant, so it runs under that tenant's context (the settings read/write and
// the account/profile ids are all tenant-scoped).
func (p *TeardownZernioProfileProcessor) Work(ctx context.Context, job *river.Job[TeardownZernioProfileTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	tid := job.Args.TenantID
	if tid == "" {
		slog.WarnContext(ctx, "teardown skipped: empty tenant_id", logging.AttrComponent, "jobs.teardown_zernio_profile")
		return nil
	}
	if p.Integration == nil || p.Settings == nil {
		slog.WarnContext(ctx, "teardown skipped: integration not wired", logging.AttrComponent, "jobs.teardown_zernio_profile", "tenant", tid)
		return nil
	}

	ctx = tenantctx.With(ctx, tid)

	// Restore-guard (CON-190): a tenant restored between the soft-delete enqueue
	// and now is active again — deleting its profile would orphan a live
	// workspace. Skip (terminal). A transient status read fails to retry rather
	// than guess. An unknown tenant (hard-deleted) reads as "not active" → proceed.
	if active, aerr := tenantIsActive(ctx, p.Tenants, tid); aerr != nil {
		return fmt.Errorf("zernio: teardown tenant status (tenant=%s): %w", tid, aerr)
	} else if active {
		slog.InfoContext(ctx, "teardown skipped: tenant active (restored)", logging.AttrComponent, "jobs.teardown_zernio_profile", "tenant", tid)
		return nil
	}

	// No key / permanently disabled: nothing to call upstream. Leave the profile
	// orphaned (the pre-CON-203 status quo) rather than burn retries.
	if !p.Integration.Enabled() {
		slog.WarnContext(ctx, "teardown skipped: integration disabled", logging.AttrComponent, "jobs.teardown_zernio_profile", "tenant", tid)
		return nil
	}

	profileID, found, err := p.Settings.Get(ctx, zernio.SettingProfileID)
	if err != nil {
		return fmt.Errorf("zernio: teardown read profile_id (tenant=%s): %w", tid, err)
	}
	if !found || profileID == "" {
		// Never provisioned (or already torn down) — nothing to delete.
		slog.InfoContext(ctx, "teardown noop: no profile_id", logging.AttrComponent, "jobs.teardown_zernio_profile", "tenant", tid)
		return nil
	}

	if err := p.teardown(ctx, profileID); err != nil {
		// Auth failures (bad/rejected key) won't be fixed by retrying — give up
		// cleanly, leaving the profile orphaned. Everything else (5xx, 429,
		// network, a lingering 400 while an account is still detaching) is
		// transient enough to let River retry with backoff.
		if zernio.IsStatus(err, http.StatusUnauthorized) || zernio.IsStatus(err, http.StatusForbidden) {
			slog.WarnContext(ctx, "teardown gave up: auth failure", logging.AttrComponent, "jobs.teardown_zernio_profile", "tenant", tid, "profile_id", profileID, logging.AttrError, err)
			return nil
		}
		return fmt.Errorf("zernio: profile teardown (tenant=%s profile=%s): %w", tid, profileID, err)
	}

	slog.InfoContext(ctx, "teardown ok", logging.AttrComponent, "jobs.teardown_zernio_profile", "tenant", tid, "profile_id", profileID)
	return nil
}

// teardown disconnects every account on the profile, then deletes the profile,
// then clears the local profile pointer. Idempotent: an already-gone account or
// profile (404) is treated as a no-op so a retry — or a re-enqueue — converges.
func (p *TeardownZernioProfileProcessor) teardown(ctx context.Context, profileID string) error {
	client := p.Integration.Client

	// Disconnect every account first: Zernio blocks profile deletion with a 400
	// while active connected accounts remain, and on delete it *moves* any
	// leftover accounts (and WhatsApp numbers) onto another profile rather than
	// dropping them — which would spawn a fresh orphan (docs.zernio.com/profiles).
	// DeleteAccount is idempotent (404 = already gone).
	accounts, err := client.ListAccounts(ctx, profileID)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	for _, a := range accounts {
		if derr := client.DeleteAccount(ctx, a.ID); derr != nil && !zernio.IsStatus(derr, http.StatusNotFound) {
			return fmt.Errorf("disconnect account %s: %w", a.ID, derr)
		}
	}

	// Delete the profile. 404 = already gone → idempotent success.
	if derr := client.DeleteProfile(ctx, profileID); derr != nil && !zernio.IsStatus(derr, http.StatusNotFound) {
		return fmt.Errorf("delete profile: %w", derr)
	}

	// Clear the local profile keys so a re-run is a clean no-op and no stale id
	// lingers. Best-effort — the upstream delete already succeeded, so a failed
	// clear must not fail (and re-run) the job.
	for _, k := range []string{
		zernio.SettingProfileID,
		zernio.SettingProfileName,
		zernio.SettingProfileCreatedAt,
		zernio.SettingProfileMeta,
	} {
		if derr := p.Settings.Delete(ctx, k); derr != nil {
			slog.WarnContext(ctx, "teardown: clear setting failed", logging.AttrComponent, "jobs.teardown_zernio_profile", "setting", k, logging.AttrError, derr)
		}
	}
	return nil
}

// Timeout is the per-attempt deadline. A teardown is a small handful of Zernio
// calls (list + a few deletes); 30s comfortably covers it, matching bootstrap.
func (p *TeardownZernioProfileProcessor) Timeout(*river.Job[TeardownZernioProfileTask]) time.Duration {
	return 30 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &TeardownZernioProfileProcessor{
			Integration: d.Integration,
			Settings:    d.AnalyticsSettings,
			Tenants:     d.Tenants,
		})
	})
}
