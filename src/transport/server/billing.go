package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/uptrace/bun"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/infra/billing/polar"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/billing"
)

// billingSettings parses the billing env. An unknown BILLING_PROVIDER or a
// missing organization id fails boot.
func billingSettings(cfg *config.Config) (billingdom.Settings, error) {
	return billingdom.ParseSettings(billingdom.SettingsInput{
		Provider: cfg.BillingProvider, TestMode: cfg.BillingTestMode,
		OrganizationID: cfg.PolarOrganizationID, OrganizationIDLive: cfg.PolarOrganizationIDLive, APIVersion: cfg.PolarAPIVersion,
		TrialTeardownLive: cfg.BillingTrialTeardownLive, AppBaseURL: cfg.AppBaseURL,
	})
}

// billingSecrets are the secrets billing needs to work at all.
var billingSecrets = []string{secrets.NamePolarAccessToken, secrets.NamePolarWebhookSecret}

// secretResolver reads a secret per call; an unset one reads as "" so the
// caller degrades instead of failing.
func secretResolver(store secrets.Store, name string) polar.SecretResolver {
	return func(ctx context.Context) (string, error) {
		v, err := store.Get(ctx, name)
		if errors.Is(err, secrets.ErrNotFound) {
			return "", nil
		}
		return v, err
	}
}

// newBillingProvider builds the Polar client over the secret store.
func newBillingProvider(cfg *config.Config, store secrets.Store, settings billingdom.Settings) billingdom.Provider {
	return polar.New(polar.Options{
		AccessToken:        secretResolver(store, secrets.NamePolarAccessToken),
		LiveAccessToken:    secretResolver(store, secrets.NamePolarAccessTokenLive),
		WebhookSecret:      secretResolver(store, secrets.NamePolarWebhookSecret),
		OrganizationID:     settings.OrganizationID,
		LiveOrganizationID: settings.OrganizationIDLive,
		TestMode:           settings.TestMode,
		APIVersion:         settings.APIVersion,
		BaseURL:            cfg.PolarBaseURL,
		Timeout:            cfg.PolarHTTPTimeout,
	})
}

// productStores holds one product store per database and mode. The HTTP app
// and the internal gRPC server each build a billing service in the same
// process; sharing the store makes a mapping saved over gRPC reach checkouts
// at once instead of after the cache TTL.
var productStores sync.Map // productStoreKey → *billing.ProductStore

type productStoreKey struct {
	db       *bun.DB
	testMode bool
}

func sharedProductStore(db *bun.DB, repo repository.BillingRepository, testMode bool) *billing.ProductStore {
	store, _ := productStores.LoadOrStore(productStoreKey{db, testMode}, billing.NewProductStore(repo, testMode))
	return store.(*billing.ProductStore)
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
	repo := repository.NewBillingRepository(db)
	return billing.New(billing.Deps{
		DB: db, Repo: repo, Tenants: repository.NewTenantRepository(db),
		Users: repository.NewUserRepository(db), Tiers: repository.NewTenantTierRepository(db),
		TierVersions: repository.NewTenantTierVersionRepository(db), Assignments: repository.NewTenantTierAssignmentRepository(db),
		Provider: provider, Settings: settings, Products: sharedProductStore(db, repo, settings.TestMode),
		Fees:     billingdom.FeeFormula{PercentBPS: rt.Config.BillingFeePercentBPS, FixedUSDCents: rt.Config.BillingFeeFixedCents},
		Notifier: rt.Notifier, Jobs: rt.Jobs, Hub: rt.Hub, Activity: rt.Activity,
		Configured: func(ctx context.Context) bool {
			for _, name := range billingSecrets {
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
	d.ensureEnqueuer()
	svc, err := NewBillingService(BillingRuntime{
		Config: d.cfg, DB: d.db, Secrets: d.secretStore, Hub: d.hub,
		Notifier: d.notifier, Jobs: d.enqueuer, Activity: d.activity.recorder,
	})
	if err != nil {
		return err
	}
	d.billing = svc
	if legacy := svc.Settings().LegacyProvider; legacy != "" {
		slog.ErrorContext(ctx, "billing: BILLING_PROVIDER names a provider this release no longer serves; billing is off until it is set to polar",
			logging.AttrComponent, "billing", "provider", legacy)
	}
	if svc.Enabled() {
		d.auth = handlers.RequireAuth(d.r.sessionRepo, d.r.userRepo, d.cfg.SessionCookieName, handlers.WithBillingGate(d.r.billingRepo.Gated))
		d.warnBillingConfig(ctx, svc)
		// Off the boot path: it reads the provider's catalog.
		go func() {
			if _, err := svc.CheckProducts(context.WithoutCancel(ctx)); err != nil {
				slog.WarnContext(ctx, "billing product check failed", logging.AttrComponent, "billing", logging.AttrError, err)
			}
		}()
	}
	return nil
}

// warnBillingConfig logs, without failing boot, what keeps billing from
// working: missing secrets, an empty product map. A dashboard edit at the
// provider must never take the API down.
func (d *deps) warnBillingConfig(ctx context.Context, svc *billing.Service) {
	for _, name := range billingSecrets {
		if v, err := secretResolver(d.secretStore, name)(ctx); err == nil && v == "" {
			slog.WarnContext(ctx, "billing enabled but secret not set; billing answers 503 until it is set in Harbor", logging.AttrComponent, "billing", "secret", name)
		}
	}
	settings := svc.Settings()
	mapped, err := svc.MappedPriceCount(ctx)
	if err != nil {
		slog.WarnContext(ctx, "billing product map unreadable", logging.AttrComponent, "billing", logging.AttrError, err)
	} else if mapped == 0 {
		slog.WarnContext(ctx, "billing enabled with no product mapped; nothing can be bought until prices are mapped in Harbor", logging.AttrComponent, "billing")
	}
	slog.InfoContext(ctx, "billing enabled", logging.AttrComponent, "billing", "provider", settings.Provider, "test_mode", settings.TestMode,
		"api_version", settings.APIVersion, "products", mapped)
}
