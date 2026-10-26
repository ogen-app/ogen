// Package billing is the billing usecase: webhook ingest and the reconcile
// sweep (both apply provider snapshots through one path), checkout, plan
// changes, and the operator actions behind Harbor.
//
// Billing never writes entitlements. It only moves a workspace between tier
// versions through TenantTierAssignmentRepository.ReassignTx, inside the same
// transaction that mirrors the subscription. Dunning is the operator's call:
// no payment status changes a tier or a tenant status on its own. The one
// automatic consequence is the unpaid-trial teardown of the Zernio profile.
package billing

import (
	"context"
	"database/sql"
	"errors"
	"expvar"
	"log/slog"
	"sync"
	"time"

	"github.com/uptrace/bun"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// Errors callers map to HTTP or gRPC answers.
var (
	// ErrUnavailable: billing is off or the provider is not configured (503).
	ErrUnavailable = errors.New("billing unavailable")
	// ErrInvalidSignature: a webhook whose signature does not match (401).
	ErrInvalidSignature = errors.New("invalid webhook signature")
	// ErrUnknownTier: the requested tier or version does not exist (400).
	ErrUnknownTier = errors.New("unknown tier")
	// ErrPriceMismatch: the price is not one of the version's prices (400).
	ErrPriceMismatch = errors.New("price does not belong to the tier")
	// ErrTierNotAvailable: the tier cannot be bought (409).
	ErrTierNotAvailable = errors.New("tier not available")
	// ErrNotChangeable: the subscription must be sorted out in the portal
	// first (409).
	ErrNotChangeable = errors.New("subscription not changeable")
	// ErrTrialChangeUnavailable: the provider refused to end a free trial
	// early (409).
	ErrTrialChangeUnavailable = errors.New("trial change unavailable")
	// ErrNoSubscription: the workspace has no subscription (404).
	ErrNoSubscription = errors.New("no subscription")
	// ErrProvider: the provider failed; nothing local changed (502).
	ErrProvider = errors.New("billing provider error")
	// ErrTenantNotFound: unknown or deleted workspace.
	ErrTenantNotFound = errors.New("tenant not found")
)

// expvar counters.
var (
	webhooksReceived      = expvar.NewInt("billing_webhooks_received")
	webhooksApplied       = expvar.NewInt("billing_webhooks_applied")
	webhooksStale         = expvar.NewInt("billing_webhooks_stale")
	webhooksFailed        = expvar.NewInt("billing_webhooks_failed")
	webhooksIgnoredMode   = expvar.NewInt("billing_webhook_mode_mismatch")
	checkoutsCreated      = expvar.NewInt("billing_checkouts_created")
	checkoutsTrial        = expvar.NewInt("billing_checkouts_created_trial")
	planChangesUpgrade    = expvar.NewInt("billing_plan_changes_upgrade")
	planChangesDowngrade  = expvar.NewInt("billing_plan_changes_downgrade")
	reconcileRuns         = expvar.NewInt("billing_reconcile_runs")
	reconcileCorrections  = expvar.NewInt("billing_reconcile_corrections")
	trialTeardownsDryRun  = expvar.NewInt("billing_trial_teardowns_dry_run")
	trialTeardownsQueued  = expvar.NewInt("billing_trial_teardowns_enqueued")
	variantPriceMismatch  = expvar.NewInt("billing_variant_price_mismatch")
	freeTrialMisconfigued = expvar.NewInt("billing_free_trial_tier_misconfigured")
)

// Notifier drops in-app notifications. *notify.Service satisfies it.
type Notifier interface {
	EmitToUsers(ctx context.Context, userIDs []string, spec notify.Spec) error
}

// Jobs enqueues the billing side effects inside the caller's transaction.
// *queues.Enqueuer satisfies it.
type Jobs interface {
	EnqueueBillingEmailTx(ctx context.Context, tx *sql.Tx, userID, tenantID, templateKey, idempotencyKey string) error
	EnqueueTrialTeardownTx(ctx context.Context, tx *sql.Tx, tenantID string) error
	EnqueueScheduledChangeStepTx(ctx context.Context, tx *sql.Tx, changeID string, at time.Time) error
}

// Deps wires a Service.
type Deps struct {
	DB           *bun.DB
	Repo         repository.BillingRepository
	Tenants      repository.TenantRepository
	Users        repository.UserRepository
	Tiers        repository.TenantTierRepository
	TierVersions repository.TenantTierVersionRepository
	Assignments  repository.TenantTierAssignmentRepository
	// Provider is nil while billing is off.
	Provider billingdom.Provider
	Settings billingdom.Settings
	// Variants serves the current mode's variant map; New builds one over
	// Repo when nil. Share one across the services of a process.
	Variants *VariantStore
	Notifier Notifier
	Jobs     Jobs
	Hub      eventhub.Hub
	Activity *activity.Recorder
	// Configured reports whether the provider's API key and webhook secret
	// are set; nil reads as configured.
	Configured func(ctx context.Context) bool
	// Now defaults to time.Now.
	Now func() time.Time
}

// Service is the billing usecase. It is safe for concurrent use.
type Service struct {
	d Deps

	variantsMu sync.Mutex
	variants   map[string]cachedVariant
}

type cachedVariant struct {
	v       billingdom.Variant
	err     error
	expires time.Time
}

// variantTTL bounds how stale a cached variant (price, trial) may be after an
// edit in the provider dashboard.
const variantTTL = 10 * time.Minute

// scheduledChangeLead is how long before the billing boundary a downgrade is
// pushed to the provider, so the renewal charges the new price.
const scheduledChangeLead = time.Hour

// New builds a Service.
func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Variants == nil {
		d.Variants = NewVariantStore(d.Repo, d.Settings.TestMode)
	}
	return &Service{d: d, variants: map[string]cachedVariant{}}
}

// Settings returns the billing settings.
func (s *Service) Settings() billingdom.Settings { return s.d.Settings }

// Configured reports whether billing is on and its secrets are set.
func (s *Service) Configured(ctx context.Context) bool {
	if !s.Enabled() {
		return false
	}
	return s.d.Configured == nil || s.d.Configured(ctx)
}

// Enabled reports whether billing is on.
func (s *Service) Enabled() bool { return s != nil && s.d.Settings.Enabled() && s.d.Provider != nil }

func (s *Service) now() time.Time { return s.d.Now().UTC() }

func (s *Service) provider() string { return models.BillingProviderLemonSqueezy }

// variant reads a provider variant through a short-lived cache.
func (s *Service) variant(ctx context.Context, id string) (billingdom.Variant, error) {
	now := s.now()
	s.variantsMu.Lock()
	c, ok := s.variants[id]
	s.variantsMu.Unlock()
	if ok && now.Before(c.expires) {
		return c.v, c.err
	}
	v, err := s.d.Provider.GetVariant(ctx, id)
	if err != nil && !errors.Is(err, billingdom.ErrNotFound) {
		// Transient failures are not cached.
		return billingdom.Variant{}, err
	}
	s.variantsMu.Lock()
	s.variants[id] = cachedVariant{v: v, err: err, expires: now.Add(variantTTL)}
	s.variantsMu.Unlock()
	return v, err
}

// publish tells the tenant's open tabs to refetch entitlements and billing.
func (s *Service) publish(ctx context.Context, tenantID string, entitlements bool) {
	if s.d.Hub == nil || tenantID == "" {
		return
	}
	topics := []string{"entity:billing:" + tenantID}
	if entitlements {
		topics = append(topics, "entity:entitlements:"+tenantID)
	}
	for _, topic := range topics {
		if err := s.d.Hub.Publish(ctx, eventhub.Event{Topic: topic, Type: "updated", TenantID: tenantID}); err != nil {
			slog.WarnContext(ctx, "billing invalidation publish failed", logging.AttrComponent, "billing", "topic", topic, logging.AttrError, err)
		}
	}
}

// notice is an owner notification queued during a transaction and emitted
// after it commits.
type notice struct {
	tenantID string
	subID    string
	typ      string
	// episode tells one occurrence of a notice type from the next (the
	// provider update, or the scheduled change), so an unread earlier notice
	// does not swallow a later event. Retries of one event keep the same one.
	episode string
	level   models.NotificationLevel
	title   string
	body    string
}

func (s *Service) emit(ctx context.Context, notices []notice) {
	for _, n := range notices {
		owners, err := s.ownerIDs(ctx, n.tenantID)
		if err != nil {
			slog.WarnContext(ctx, "billing notice: owner lookup failed", logging.AttrComponent, "billing", "tenant_id", n.tenantID, logging.AttrError, err)
			continue
		}
		tctx := tenantctx.With(ctx, n.tenantID)
		if err := s.d.Notifier.EmitToUsers(tctx, owners, notify.Spec{
			Level:      n.level,
			Type:       "billing." + n.typ,
			Title:      n.title,
			Body:       n.body,
			EntityType: "billing",
			EntityID:   n.subID,
			ActionURL:  "/plans",
			DedupeKey:  "billing:" + n.subID + ":" + n.typ + ":" + n.episode,
		}); err != nil {
			slog.WarnContext(ctx, "billing notice emit failed", logging.AttrComponent, "billing", "tenant_id", n.tenantID, "type", n.typ, logging.AttrError, err)
		}
		if s.d.Activity != nil {
			s.d.Activity.Record(tctx, activity.CategoryBilling, n.typ,
				activity.WithSource(activity.SourceSystem), activity.WithEntity("billing_subscription", n.subID))
		}
	}
}

func (s *Service) ownerIDs(ctx context.Context, tenantID string) ([]string, error) {
	owners, err := s.d.Users.ListOwnersByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(owners))
	for _, o := range owners {
		ids = append(ids, o.ID)
	}
	return ids, nil
}

// liveSubscription returns the subscription a tenant follows: the most
// recently created one that is neither invalidated nor in the other mode, or
// nil. Older live ones are duplicates.
func (s *Service) liveSubscription(subs []models.BillingSubscription) *models.BillingSubscription {
	now := s.now()
	for i := range subs {
		sub := &subs[i]
		if sub.InvalidatedAt == nil && sub.TestMode == s.d.Settings.TestMode && billingdom.Live(sub.Status, sub.EndsAt, now) {
			return sub
		}
	}
	return nil
}

// currentSubscription is the tenant's latest subscription in the current
// mode, live or not (an expired one still shows on the billing card).
func (s *Service) currentSubscription(subs []models.BillingSubscription) *models.BillingSubscription {
	if live := s.liveSubscription(subs); live != nil {
		return live
	}
	for i := range subs {
		sub := &subs[i]
		if sub.InvalidatedAt == nil && sub.TestMode == s.d.Settings.TestMode {
			return sub
		}
	}
	return nil
}

// HasLiveSubscription implements queues.BillingLiveChecker.
func (s *Service) HasLiveSubscription(ctx context.Context, tenantID string) (bool, error) {
	subs, err := s.d.Repo.ListSubscriptionsByTenant(ctx, nil, tenantID)
	if err != nil {
		return false, err
	}
	return s.liveSubscription(subs) != nil, nil
}
