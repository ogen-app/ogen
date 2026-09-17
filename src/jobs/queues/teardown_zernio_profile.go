package queues

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// TenantTeardownFence serializes the teardown against a concurrent tenant
// restore (CON-190). It runs the destructive work while holding the tenant row
// lock and hands back the status read under that lock, so SetStatus(active)
// cannot restore the tenant mid-teardown. repository.TenantTeardownFence
// implements it; a narrow interface here keeps the worker off the repository
// package and unit-testable. See src/infra/repository/tenant_teardown_fence.go.
type TenantTeardownFence interface {
	WithTenantLock(ctx context.Context, tenantID string, fn func(ctx context.Context, status string) error) error
}

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
	// Fence serializes the teardown against a concurrent CON-190 restore by
	// holding the tenant row lock across the destructive Zernio calls (and
	// surfacing the status read under that lock). Fail-closed — a nil fence
	// (unwired) skips teardown rather than delete an unfenced profile. In prod it
	// is always wired (repository.NewTenantTeardownFence).
	Fence TenantTeardownFence
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
	if p.Integration == nil || p.Settings == nil || p.Fence == nil {
		slog.WarnContext(ctx, "teardown skipped: integration not wired", logging.AttrComponent, "jobs.teardown_zernio_profile", "tenant", tid)
		return nil
	}

	ctx = tenantctx.With(ctx, tid)

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

	// Run the destructive teardown under the tenant row lock, re-reading the
	// status under it. This fences the whole disconnect+delete sequence against a
	// concurrent CON-190 restore: SetStatus(active) takes the same lock, so it
	// cannot restore the tenant mid-teardown. If a restore committed first, we
	// observe 'active' here and skip — never deleting a live tenant's profile.
	// (A hard-deleted tenant yields status "" and no lock; nothing can restore
	// it, so proceeding is safe.)
	err = p.Fence.WithTenantLock(ctx, tid, func(ctx context.Context, status string) error {
		if status == models.TenantStatusActive {
			slog.InfoContext(ctx, "teardown skipped: tenant active (restored)", logging.AttrComponent, "jobs.teardown_zernio_profile", "tenant", tid)
			return nil
		}
		return p.teardown(ctx, profileID)
	})
	if err != nil {
		// Auth failures (bad/rejected key) won't be fixed by retrying — give up
		// cleanly, leaving the profile orphaned. Everything else (5xx, 429,
		// network, a lingering 400 while an account is still detaching, or a
		// transient DB error taking the lock) is transient enough to let River
		// retry with backoff.
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
	//
	// A 404 from ListAccounts means the profile itself is already gone upstream
	// (a prior attempt deleted it but failed to clear local settings, so the job
	// retried). That is not an error: fall through to local cleanup so the retry
	// converges instead of erroring on every attempt until it dies.
	accounts, err := client.ListAccounts(ctx, profileID)
	switch {
	case err == nil:
		for _, a := range accounts {
			if derr := client.DeleteAccount(ctx, a.ID); derr != nil && !zernio.IsStatus(derr, http.StatusNotFound) {
				return fmt.Errorf("disconnect account %s: %w", a.ID, derr)
			}
		}
		// Delete the profile. 404 = already gone → idempotent success.
		if derr := client.DeleteProfile(ctx, profileID); derr != nil && !zernio.IsStatus(derr, http.StatusNotFound) {
			return fmt.Errorf("delete profile: %w", derr)
		}
	case zernio.IsStatus(err, http.StatusNotFound):
		// Profile already deleted upstream — nothing to disconnect/delete.
	default:
		return fmt.Errorf("list accounts: %w", err)
	}

	// Clear the local profile keys so a re-run is a clean no-op and no stale id
	// lingers. Unlike the remote deletes, a failure here MUST fail the job so
	// River retries — otherwise a dangling profile_id survives a "successful"
	// teardown. Attempt every key first (so a mid-list failure still clears the
	// rest), then surface the first error. On retry, ListAccounts 404s (profile
	// already gone) and we return straight here, so the retry converges once the
	// clear finally lands.
	var clearErr error
	for _, k := range []string{
		zernio.SettingProfileID,
		zernio.SettingProfileName,
		zernio.SettingProfileCreatedAt,
		zernio.SettingProfileMeta,
	} {
		if derr := p.Settings.Delete(ctx, k); derr != nil {
			slog.WarnContext(ctx, "teardown: clear setting failed", logging.AttrComponent, "jobs.teardown_zernio_profile", "setting", k, logging.AttrError, derr)
			if clearErr == nil {
				clearErr = derr
			}
		}
	}
	if clearErr != nil {
		return fmt.Errorf("clear profile settings: %w", clearErr)
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
			Fence:       d.TenantFence,
		})
	})
}
