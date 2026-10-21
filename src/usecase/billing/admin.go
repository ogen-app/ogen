package billing

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/uptrace/bun"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email/templates"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Operator errors.
var (
	// ErrStillTestMode: the cutover runs only once the server is live.
	ErrStillTestMode = errors.New("billing still runs in test mode")
	// ErrTrialVariantMissing: the tier cannot carry the free month.
	ErrTrialVariantMissing = errors.New("tier has no one-month trial variant for every mapped price")
	// ErrReasonRequired: operator actions must say why.
	ErrReasonRequired = errors.New("reason is required")
)

const (
	recentWebhookEvents = 20
	attentionWindow     = 30 * 24 * time.Hour
)

// Attention reasons.
const (
	AttentionPastDue               = models.SubscriptionStatusPastDue
	AttentionUnpaid                = models.SubscriptionStatusUnpaid
	AttentionPaused                = models.SubscriptionStatusPaused
	AttentionExpired               = models.SubscriptionStatusExpired
	AttentionExpiredNeverPaid      = "expired_never_paid"
	AttentionDuplicate             = "duplicate"
	AttentionUnresolvedWebhook     = "unresolved_webhook"
	AttentionScheduledChangeFailed = "scheduled_change_failed"
)

// OwnerTrial is an owner's free-month eligibility.
type OwnerTrial struct {
	UserID        string
	AccountID     string
	Email         string
	TrialEligible bool
}

// TenantBilling is the operator's view of one workspace.
type TenantBilling struct {
	TenantID         string
	BillingRequired  bool
	Gated            bool
	Subscriptions    []models.BillingSubscription
	Duplicates       map[string]bool
	ScheduledChanges []models.BillingScheduledChange
	WebhookEvents    []models.BillingWebhookEvent
	Owners           []OwnerTrial
	Prices           map[string]billingdom.Price
}

// TenantBilling reads one workspace's billing picture.
func (s *Service) TenantBilling(ctx context.Context, tenantID string) (TenantBilling, error) {
	tenant, err := s.d.Tenants.GetByID(ctx, tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return TenantBilling{}, ErrTenantNotFound
	}
	if err != nil {
		return TenantBilling{}, err
	}
	out := TenantBilling{TenantID: tenantID, BillingRequired: tenant.BillingRequired, Duplicates: map[string]bool{}}
	if out.Gated, err = s.d.Repo.Gated(ctx, tenantID); err != nil {
		return out, err
	}
	if out.Subscriptions, err = s.d.Repo.ListSubscriptionsByTenant(ctx, nil, tenantID); err != nil {
		return out, err
	}
	followed := s.liveSubscription(out.Subscriptions)
	var priceIDs []string
	for _, sub := range out.Subscriptions {
		if followed != nil && sub.ID != followed.ID && sub.InvalidatedAt == nil && billingdom.Live(sub.Status, sub.EndsAt, s.now()) {
			out.Duplicates[sub.ID] = true
		}
		priceIDs = append(priceIDs, deref(sub.TierVersionPriceID))
	}
	if out.Prices, err = s.d.Repo.Prices(ctx, compact(priceIDs...)); err != nil {
		return out, err
	}
	if out.ScheduledChanges, err = s.d.Repo.ListScheduledChangesByTenant(ctx, tenantID); err != nil {
		return out, err
	}
	if out.WebhookEvents, err = s.d.Repo.ListWebhookEventsByTenant(ctx, tenantID, recentWebhookEvents); err != nil {
		return out, err
	}
	owners, err := s.d.Users.ListOwnersByTenant(ctx, tenantID)
	if err != nil {
		return out, err
	}
	for _, o := range owners {
		eligible, err := s.TrialEligible(ctx, o.AccountID)
		if err != nil {
			return out, err
		}
		out.Owners = append(out.Owners, OwnerTrial{UserID: o.ID, AccountID: o.AccountID, Email: o.Email, TrialEligible: eligible})
	}
	return out, nil
}

// AttentionItem is one reason a workspace needs an operator.
type AttentionItem struct {
	TenantID       string
	TenantName     string
	Reason         string
	Subscription   *models.BillingSubscription
	WebhookEvent   *models.BillingWebhookEvent
	Since          time.Time
	TeardownStatus string
}

// Attention lists workspaces that need an operator, newest first. The page
// token is an offset; the list is small by nature.
func (s *Service) Attention(ctx context.Context, pageSize int, pageToken string) ([]AttentionItem, string, error) {
	pageSize = min(cmp.Or(pageSize, 50), 200)
	if pageSize <= 0 {
		pageSize = 50
	}
	offset, _ := strconv.Atoi(pageToken)
	since := s.now().Add(-attentionWindow)
	mode := s.d.Settings.TestMode
	var items []AttentionItem

	troubled, err := s.d.Repo.ListByStatus(ctx, mode, []string{models.SubscriptionStatusPastDue, models.SubscriptionStatusUnpaid, models.SubscriptionStatusPaused}, nil)
	if err != nil {
		return nil, "", err
	}
	for i := range troubled {
		items = append(items, AttentionItem{TenantID: troubled[i].TenantID, Reason: troubled[i].Status, Subscription: &troubled[i], Since: troubled[i].ProviderUpdatedAt})
	}
	expired, err := s.d.Repo.ListByStatus(ctx, mode, []string{models.SubscriptionStatusExpired}, &since)
	if err != nil {
		return nil, "", err
	}
	for i := range expired {
		sub := &expired[i]
		item := AttentionItem{TenantID: sub.TenantID, Reason: AttentionExpired, Subscription: sub, Since: sub.ProviderUpdatedAt}
		if sub.FirstPaidAt == nil {
			item.Reason = AttentionExpiredNeverPaid
			item.TeardownStatus = "dry_run"
			if sub.TeardownAt != nil {
				item.TeardownStatus = "enqueued"
			}
		}
		items = append(items, item)
	}
	live, err := s.d.Repo.ListByStatus(ctx, mode, []string{models.SubscriptionStatusOnTrial, models.SubscriptionStatusActive, models.SubscriptionStatusPastDue,
		models.SubscriptionStatusUnpaid, models.SubscriptionStatusPaused, models.SubscriptionStatusCancelled}, nil)
	if err != nil {
		return nil, "", err
	}
	byTenant := map[string][]models.BillingSubscription{}
	for _, sub := range live {
		if billingdom.Live(sub.Status, sub.EndsAt, s.now()) {
			byTenant[sub.TenantID] = append(byTenant[sub.TenantID], sub)
		}
	}
	for _, subs := range byTenant {
		if len(subs) < 2 {
			continue
		}
		slices.SortFunc(subs, func(a, b models.BillingSubscription) int { return b.CreatedAt.Compare(a.CreatedAt) })
		for i := range subs[1:] {
			dup := subs[1+i]
			items = append(items, AttentionItem{TenantID: dup.TenantID, Reason: AttentionDuplicate, Subscription: &dup, Since: subs[0].CreatedAt})
		}
	}
	unresolved, err := s.d.Repo.ListFailedWebhookEvents(ctx, since, true)
	if err != nil {
		return nil, "", err
	}
	for i := range unresolved {
		items = append(items, AttentionItem{Reason: AttentionUnresolvedWebhook, WebhookEvent: &unresolved[i], Since: unresolved[i].ReceivedAt})
	}
	failed, err := s.d.Repo.ListScheduledChangesByStatus(ctx, models.ScheduledChangeFailed, since)
	if err != nil {
		return nil, "", err
	}
	for _, c := range failed {
		sub, err := s.d.Repo.GetSubscription(ctx, c.BillingSubscriptionID)
		if err != nil {
			continue
		}
		items = append(items, AttentionItem{TenantID: c.TenantID, Reason: AttentionScheduledChangeFailed, Subscription: sub, Since: c.UpdatedAt})
	}

	slices.SortStableFunc(items, func(a, b AttentionItem) int { return b.Since.Compare(a.Since) })
	var ids []string
	for _, it := range items {
		ids = append(ids, it.TenantID)
	}
	names, err := s.d.Repo.TenantNames(ctx, compact(ids...))
	if err != nil {
		return nil, "", err
	}
	for i := range items {
		items[i].TenantName = names[items[i].TenantID]
	}
	if offset >= len(items) {
		return nil, "", nil
	}
	end := min(offset+pageSize, len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[offset:end], next, nil
}

// SetBillingRequired comps or exempts a workspace, or puts it back behind the
// gate.
func (s *Service) SetBillingRequired(ctx context.Context, tenantID string, required bool, reason string) error {
	if reason == "" {
		return ErrReasonRequired
	}
	ok, err := s.d.Repo.SetBillingRequired(ctx, tenantID, required, s.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrTenantNotFound
	}
	if s.d.Activity != nil {
		s.d.Activity.Record(tenantctx.With(ctx, tenantID), activity.CategoryBilling, "billing_required_set",
			activity.WithSource(activity.SourceSystem), activity.WithPayload(map[string]any{"required": required, "reason": reason}))
	}
	s.publish(ctx, tenantID, true)
	return nil
}

// FreeTrialOffer describes the marketing free month.
type FreeTrialOffer struct {
	TierID          string
	TierName        string
	TrialDays       int
	EligibleTierIDs []string
}

// FreeTrialOffer reads the marked tier and the tiers that could be marked.
func (s *Service) FreeTrialOffer(ctx context.Context) (FreeTrialOffer, error) {
	var out FreeTrialOffer
	if !s.Enabled() {
		return out, nil
	}
	tiers, err := s.d.Tiers.List(ctx)
	if err != nil {
		return out, err
	}
	for _, t := range tiers {
		days, err := s.tierTrialDays(ctx, t.ID)
		if err != nil {
			return out, err
		}
		if days > 0 {
			out.EligibleTierIDs = append(out.EligibleTierIDs, t.ID)
		}
		if t.FreeTrial {
			out.TierID, out.TierName, out.TrialDays = t.ID, t.Name, days
		}
	}
	return out, nil
}

// tierTrialDays returns the free-month length when every mapped price of the
// tier's current purchasable version has a one-month trial variant, else 0.
func (s *Service) tierTrialDays(ctx context.Context, tierID string) (int, error) {
	version, err := s.d.TierVersions.LatestActiveByTier(ctx, tierID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil || !version.Purchasable {
		return 0, err
	}
	rows, err := s.d.TierVersions.PricesByVersion(ctx, version.ID)
	if err != nil {
		return 0, err
	}
	days, mapped := 0, 0
	for _, r := range rows {
		variantID, ok := s.d.Settings.Variants.Variant(r.ID)
		if !ok {
			continue
		}
		mapped++
		v, err := s.variant(ctx, variantID)
		if errors.Is(err, billingdom.ErrNotFound) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		if !v.OneMonthTrial() {
			return 0, nil
		}
		days = v.TrialDays()
	}
	if mapped == 0 {
		return 0, nil
	}
	return days, nil
}

// SetFreeTrialTier marks the free-month tier (clearing the previous one), or
// turns the offer off with an empty id. New checkouts only.
func (s *Service) SetFreeTrialTier(ctx context.Context, tierID, reason string) (FreeTrialOffer, error) {
	if reason == "" {
		return FreeTrialOffer{}, ErrReasonRequired
	}
	if !s.Enabled() {
		return FreeTrialOffer{}, ErrUnavailable
	}
	if tierID != "" {
		if _, err := s.d.Tiers.GetByID(ctx, tierID); errors.Is(err, sql.ErrNoRows) {
			return FreeTrialOffer{}, ErrUnknownTier
		} else if err != nil {
			return FreeTrialOffer{}, err
		}
		days, err := s.tierTrialDays(ctx, tierID)
		if err != nil {
			return FreeTrialOffer{}, providerErr(err)
		}
		if days == 0 {
			return FreeTrialOffer{}, ErrTrialVariantMissing
		}
	}
	ok, err := s.d.Repo.SetFreeTrialTier(ctx, tierID, s.now())
	if err != nil {
		return FreeTrialOffer{}, err
	}
	if !ok {
		return FreeTrialOffer{}, ErrUnknownTier
	}
	// An operator-wide setting, so it is audited in the log rather than in a
	// workspace's activity.
	slog.InfoContext(ctx, "billing: free-month tier changed", logging.AttrComponent, "billing", "tier_id", tierID, "reason", reason)
	return s.FreeTrialOffer(ctx)
}

// InvalidateTestSubscriptions is the sandbox-to-live cutover: every test-mode
// subscription is invalidated, its workspace re-gated (when billing_required),
// and its owners asked to check out again. Refused while still in test mode.
func (s *Service) InvalidateTestSubscriptions(ctx context.Context, dryRun bool) ([]string, error) {
	if !s.Enabled() {
		return nil, ErrUnavailable
	}
	if s.d.Settings.TestMode {
		return nil, ErrStillTestMode
	}
	subs, err := s.d.Repo.ListTestModeLive(ctx)
	if err != nil {
		return nil, err
	}
	byTenant := map[string][]string{}
	var tenants []string
	for _, sub := range subs {
		if _, ok := byTenant[sub.TenantID]; !ok {
			tenants = append(tenants, sub.TenantID)
		}
		byTenant[sub.TenantID] = append(byTenant[sub.TenantID], sub.ID)
	}
	if dryRun {
		return tenants, nil
	}
	for _, tenantID := range tenants {
		err := s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if err := s.d.Repo.Invalidate(ctx, tx, byTenant[tenantID], models.InvalidatedReasonModeCutover, s.now()); err != nil {
				return err
			}
			owners, err := s.d.Users.ListOwnersByTenant(ctx, tenantID)
			if err != nil {
				return err
			}
			for _, o := range owners {
				key := templates.KeyBillingReentryRequired + ":" + tenantID + ":" + o.ID
				if err := s.d.Jobs.EnqueueBillingEmailTx(ctx, tx.Tx, o.ID, tenantID, templates.KeyBillingReentryRequired, key); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		s.emit(ctx, []notice{{tenantID: tenantID, subID: byTenant[tenantID][0], typ: "reentry_required", level: models.NotificationLevelWarning,
			title: "Please re-enter your payment details", body: "Billing moved from its test period to live payments. Choose your plan again to keep going."}})
		s.publish(ctx, tenantID, true)
	}
	return tenants, nil
}

// BillingStatus summarises a workspace for the operator tenant list: empty when
// billing is off; not_required; awaiting_checkout; or the provider status of
// its current subscription.
func (s *Service) BillingStatus(ctx context.Context, t *models.Tenant) (string, error) {
	if !s.Enabled() {
		return "", nil
	}
	subs, err := s.d.Repo.ListSubscriptionsByTenant(ctx, nil, t.ID)
	if err != nil {
		return "", err
	}
	if sub := s.currentSubscription(subs); sub != nil {
		return sub.Status, nil
	}
	if t.BillingRequired {
		return "awaiting_checkout", nil
	}
	return "not_required", nil
}

// PricingFlags is what the public pricing read adds per tier and price.
type PricingFlags struct {
	Enabled bool
	// MappedPrices are prices with a provider variant.
	MappedPrices map[string]bool
	// FreeTrialTierID is the marked tier when it can carry the free month.
	FreeTrialTierID string
	TrialDays       int
}

// PricingFlags reads the purchasability flags for public pricing.
func (s *Service) PricingFlags(ctx context.Context) (PricingFlags, error) {
	out := PricingFlags{MappedPrices: map[string]bool{}}
	if !s.Enabled() {
		return out, nil
	}
	out.Enabled = true
	for _, id := range s.d.Settings.Variants.PriceIDs() {
		out.MappedPrices[id] = true
	}
	marked, err := s.d.Repo.FreeTrialTier(ctx)
	if err != nil || marked == nil {
		return out, err
	}
	days, err := s.tierTrialDays(ctx, marked.ID)
	if err != nil {
		// Pricing must render even when the provider is unreachable; the
		// checkout re-checks the trial on its own.
		return out, nil
	}
	if days > 0 {
		out.FreeTrialTierID, out.TrialDays = marked.ID, days
	}
	return out, nil
}
