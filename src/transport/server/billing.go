package server

import (
	"context"
	"errors"
	"log/slog"

	"github.com/uptrace/bun"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/infra/billing/lemonsqueezy"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/billing"
)

// billingSettings parses the billing env. A bad BILLING_PROVIDER, a missing
// store id or a malformed variant map fails boot.
func billingSettings(cfg *config.Config) (billingdom.Settings, error) {
	return billingdom.ParseSettings(cfg.BillingProvider, cfg.LemonSqueezyTestMode, cfg.LemonSqueezyStoreID,
		cfg.LemonSqueezyVariants, cfg.BillingTrialTeardownLive, cfg.AppBaseURL)
}

// secretResolver reads a secret per call; an unset one reads as "" so the
// caller degrades instead of failing.
func secretResolver(store secrets.Store, name string) lemonsqueezy.SecretResolver {
	return func(ctx context.Context) (string, error) {
		v, err := store.Get(ctx, name)
		if errors.Is(err, secrets.ErrNotFound) {
			return "", nil
		}
		return v, err
	}
}

// newBillingProvider builds the Lemon Squeezy client over the secret store.
func newBillingProvider(cfg *config.Config, store secrets.Store, settings billingdom.Settings) billingdom.Provider {
	return lemonsqueezy.New(
		secretResolver(store, secrets.NameLemonSqueezyAPIKey),
		secretResolver(store, secrets.NameLemonSqueezyWebhookSecret),
		settings.StoreID, cfg.LemonSqueezyBaseURL, cfg.LemonSqueezyHTTPTimeout)
}

// BillingRuntime is what the billing usecase needs from the process.
type BillingRuntime struct {
	Config   *config.Config
	DB       *bun.DB
	Secrets  secrets.Store
	Hub      eventhub.Hub
	Notifier billing.Notifier
	Jobs     billing.Jobs
	Activity *activity.Recorder
}

// NewBillingService builds the billing usecase. The HTTP app and the
// internal gRPC server each build one over the same database.
func NewBillingService(rt BillingRuntime) (*billing.Service, error) {
	settings, err := billingSettings(rt.Config)
	if err != nil {
		return nil, err
	}
	var provider billingdom.Provider
	if settings.Enabled() {
		provider = newBillingProvider(rt.Config, rt.Secrets, settings)
	}
	db := rt.DB
	return billing.New(billing.Deps{
		DB: db, Repo: repository.NewBillingRepository(db), Tenants: repository.NewTenantRepository(db),
		Users: repository.NewUserRepository(db), Tiers: repository.NewTenantTierRepository(db),
		TierVersions: repository.NewTenantTierVersionRepository(db), Assignments: repository.NewTenantTierAssignmentRepository(db),
		Provider: provider, Settings: settings, Notifier: rt.Notifier, Jobs: rt.Jobs,
		Hub: rt.Hub, Activity: rt.Activity,
		Configured: func(ctx context.Context) bool {
			for _, name := range []string{secrets.NameLemonSqueezyAPIKey, secrets.NameLemonSqueezyWebhookSecret} {
				if v, err := secretResolver(rt.Secrets, name)(ctx); err != nil || v == "" {
					return false
				}
			}
			return true
		},
	}), nil
}

// initBilling builds the billing usecase and, when billing is on, turns the
// gate on for the cookie chain. It runs before River: the billing jobs are
// registered with it, and it enqueues through the same enqueuer River gets.
func (d *deps) initBilling(ctx context.Context) error {
	if d.enqueuer == nil {
		d.enqueuer = &queues.Enqueuer{}
	}
	svc, err := NewBillingService(BillingRuntime{
		Config: d.cfg, DB: d.db, Secrets: d.secretStore, Hub: d.hub,
		Notifier: d.notifier, Jobs: d.enqueuer, Activity: d.activity.recorder,
	})
	if err != nil {
		return err
	}
	d.billing = svc
	if svc.Enabled() {
		d.auth = handlers.RequireAuth(d.r.sessionRepo, d.r.userRepo, d.cfg.SessionCookieName, handlers.WithBillingGate(d.r.billingRepo.Gated))
		d.warnBillingConfig(ctx, svc.Settings())
		// Off the boot path: it calls the provider once per mapped variant.
		go func() {
			if _, err := svc.CheckVariants(context.WithoutCancel(ctx)); err != nil {
				slog.WarnContext(ctx, "billing variant check failed", logging.AttrComponent, "billing", logging.AttrError, err)
			}
		}()
	}
	return nil
}

// warnBillingConfig logs, without failing boot, what keeps billing from
// working: missing secrets, a variant map that references no sellable price.
// An LS dashboard edit must never take the API down.
func (d *deps) warnBillingConfig(ctx context.Context, settings billingdom.Settings) {
	for _, name := range []string{secrets.NameLemonSqueezyAPIKey, secrets.NameLemonSqueezyWebhookSecret} {
		if v, err := secretResolver(d.secretStore, name)(ctx); err == nil && v == "" {
			slog.WarnContext(ctx, "billing enabled but secret not set; billing answers 503 until it is set in Harbor", logging.AttrComponent, "billing", "secret", name)
		}
	}
	if settings.Variants.Len() == 0 {
		slog.WarnContext(ctx, "billing enabled with an empty LEMONSQUEEZY_VARIANTS; nothing can be bought", logging.AttrComponent, "billing")
	}
	slog.InfoContext(ctx, "billing enabled", logging.AttrComponent, "billing", "provider", settings.Provider, "test_mode", settings.TestMode, "variants", settings.Variants.Len())
}
