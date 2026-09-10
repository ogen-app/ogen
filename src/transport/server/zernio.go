package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// initZernio constructs the Zernio integration, Bootstrapper, sync worker, and
// a SettingsStore adapter wrapping the host's SettingRepository. The full
// runtime is always wired — even with no API key at boot — so the routes are
// always registered and a key set later via the secrets API enables the
// integration with no reboot. Ping/warmup runs in a background goroutine, so
// Ogen boot never blocks on Zernio reachability.
//
// State, set by warmupZernio at boot and re-evaluated whenever zernio_api_key
// changes (secrets subscription below):
//
//   - No API key            → StateDisabled (endpoints 409, worker idle).
//   - Ping returns 401      → StateDisabled; admin repairs via /profile/repair.
//   - Ping transport / 5xx  → StateDegraded; retried by the worker.
//   - Ping returns 200      → StateOK; per-tenant profiles bootstrap lazily.
//
// zernioRuntime bundles the long-lived collaborators. All fields are non-nil;
// usability is reflected by Integration.State()/Enabled(), not by which
// collaborators exist.
type zernioRuntime struct {
	Integration  *zernio.Integration
	Bootstrapper *zernio.Bootstrapper
	Settings     zernio.SettingsStore
	RateLimiter  *zernio.RateLimiter
	Worker       *zernio.Worker
	WorkerCancel context.CancelFunc
}

func initZernio(
	ctx context.Context,
	cfg *config.Config,
	secretStore secrets.Store,
	settingRepo repository.SettingRepository,
	accountRepo repository.SocialAccountRepository,
	hub eventhub.Hub,
	recorder *usage.Recorder,
) zernioRuntime {
	// 30s in-process TTL cache around zernio.* setting reads. All
	// downstream callers (handlers, bootstrap, worker) share this
	// single instance so writes invalidate everyone's view.
	store := zernio.NewCachedSettingsStore(&settingsStoreAdapter{repo: settingRepo})

	// The integration is always wired, even with no key at boot: the client
	// resolves the zernio_api_key per call, so a key set/rotated/cleared via the
	// secrets API takes effect with no reboot. warmupZernio sets the state
	// (disabled when no key) and the subscription below re-validates on change.
	resolver := func(ctx context.Context) (string, error) {
		return secretStore.Get(ctx, secrets.NameZernioAPIKey)
	}
	client := zernio.NewClient(resolver, cfg.ZernioBaseURL, zernio.ClientOpts{
		Timeout:     cfg.ZernioHTTPTimeout,
		RedirectURL: cfg.ZernioRedirectURL,
	})
	integ := zernio.NewIntegration(client)
	bootstrapper := zernio.NewBootstrapper(integ, store, cfg.ZernioEnv)
	// CON-102: sweep tenants that have INITIATED a connection, not merely those
	// with a profile. Eager provisioning gives every tenant a zernio.profile_id
	// at signup, so keying the sweep on profile presence would poll every tenant
	// each tick — even ones that never connected. The connect_initiated_at marker
	// (written on the first connect-link) is the effective "uses Zernio" signal.
	worker := zernio.NewWorker(integ, accountRepo, store, hub, bootstrapper, recorder, cfg.ZernioSyncInterval, cfg.ZernioSyncIntervalFast, func(ctx context.Context) ([]string, error) {
		return settingRepo.ListTenantIDsByKey(ctx, zernio.SettingConnectInitiatedAt)
	})

	workerCtx, workerCancel := context.WithCancel(ctx)
	// CON-97: the Zernio bootstrap + sync worker run outside any request and
	// legitimately span tenants (reading the profile setting, syncing social
	// accounts), so they run on a system context. Per-tenant Zernio sync is a
	// follow-up (CON-97 §10.4).
	workerCtx = tenantctx.WithSystem(workerCtx)

	// warmupEpoch orders concurrent warmups (boot + each key change). A warmup
	// captures the epoch at its start and only writes state if it's still the
	// latest, so a slow goroutine from an older change can't overwrite a newer
	// clear/reject result.
	warmupEpoch := &atomic.Uint64{}
	go warmupZernio(workerCtx, integ, secretStore, warmupEpoch, warmupEpoch.Add(1))
	go worker.Run(workerCtx)

	// Hot-reload: setting / rotating / clearing zernio_api_key via the secrets
	// API re-validates the key and flips the integration state (and kicks a
	// sync) with no reboot — mirroring the Anthropic genkit-rebuild
	// subscription in genkit_runtime.go. store.notify invokes this on the Set
	// path, so run the network ping off a goroutine to avoid blocking it. The
	// token is taken synchronously (in Set order) so the latest change wins.
	secretStore.Subscribe(secrets.NameZernioAPIKey, func() {
		token := warmupEpoch.Add(1)
		go func() {
			warmupZernio(workerCtx, integ, secretStore, warmupEpoch, token)
			worker.TriggerNow()
		}()
	})

	return zernioRuntime{
		Integration:  integ,
		Bootstrapper: bootstrapper,
		Settings:     store,
		RateLimiter:  zernio.NewConnectLinkRateLimiter(),
		Worker:       worker,
		WorkerCancel: workerCancel,
	}
}

// shutdown signals the worker to stop and waits up to 2s for the
// goroutine to exit cleanly. Wired from a Fiber OnShutdown hook.
func (r zernioRuntime) shutdown() {
	if r.WorkerCancel == nil || r.Worker == nil {
		return
	}
	r.WorkerCancel()
	select {
	case <-r.Worker.Done():
	case <-time.After(2 * time.Second):
		slog.Warn("worker did not exit within 2s; leaving it to the process",
			logging.AttrComponent, "zernio")
	}
}

// warmupZernio resolves the desired integration state from the current
// zernio_api_key + a warmup Ping, then writes it only if no newer key change
// has superseded this run (epoch == token). That gate stops a slow goroutine
// from an older change overwriting a newer clear/reject result. Per-tenant
// profiles are bootstrapped lazily on connect, not here; the goroutine exits
// cleanly on ctx cancellation (process shutdown).
func warmupZernio(ctx context.Context, integ *zernio.Integration, secretStore secrets.Store, epoch *atomic.Uint64, token uint64) {
	desired := resolveZernioState(ctx, integ, secretStore)
	if epoch.Load() == token {
		integ.SetState(desired)
	}
}

// resolveZernioState computes the integration state without mutating it. A
// missing key → disabled; a transient secret-read failure → degraded
// (retryable — the worker keeps retrying, rather than disabling until the next
// key change or reboot); a 401 → disabled; other ping errors → degraded; a
// successful ping → ok (each tenant's profile is created lazily on connect).
func resolveZernioState(ctx context.Context, integ *zernio.Integration, secretStore secrets.Store) zernio.State {
	if _, err := secretStore.Get(ctx, secrets.NameZernioAPIKey); err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			slog.WarnContext(ctx, "integration disabled; zernio_api_key not set",
				logging.AttrComponent, "zernio")
			return zernio.StateDisabled
		}
		slog.WarnContext(ctx, "read zernio_api_key failed; staying degraded",
			logging.AttrComponent, "zernio",
			logging.AttrError, err)
		return zernio.StateDegraded
	}
	if err := integ.Client.Ping(ctx); err != nil {
		if zernio.IsStatus(err, http.StatusUnauthorized) {
			slog.WarnContext(ctx, "api key rejected (401); integration disabled until repaired",
				logging.AttrComponent, "zernio")
			return zernio.StateDisabled
		}
		if apiErr, ok := errors.AsType[*zernio.APIError](err); ok {
			slog.WarnContext(ctx, "ping failed; staying degraded",
				logging.AttrComponent, "zernio",
				"status", apiErr.Status)
		} else {
			slog.WarnContext(ctx, "ping failed; staying degraded",
				logging.AttrComponent, "zernio",
				logging.AttrError, err)
		}
		return zernio.StateDegraded
	}
	slog.InfoContext(ctx, "api key validated",
		logging.AttrComponent, "zernio",
		"base", integ.Client.BaseURL())
	return zernio.StateOK
}

// settingsStoreAdapter bridges repository.SettingRepository to the
// narrow SettingsStore contract the zernio package consumes.
type settingsStoreAdapter struct {
	repo repository.SettingRepository
}

func (a *settingsStoreAdapter) Get(ctx context.Context, key string) (string, bool, error) {
	s, err := a.repo.GetByKey(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return s.Value, true, nil
}

func (a *settingsStoreAdapter) Set(ctx context.Context, key, value string) error {
	return a.repo.Upsert(ctx, &models.Setting{Key: key, Value: value})
}

func (a *settingsStoreAdapter) Delete(ctx context.Context, key string) error {
	_, err := a.repo.Delete(ctx, key)
	return err
}
