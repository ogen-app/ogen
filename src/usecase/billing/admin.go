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
	// ErrStillTestMode: the mode cutover runs only once the server is live.
	ErrStillTestMode = errors.New("billing still runs in test mode")
	// ErrTrialProductMissing: the tier cannot carry the free month.
	ErrTrialProductMissing = errors.New("tier has no one-month trial product for every mapped price")
	// ErrReasonRequired: operator actions must say why.
	ErrReasonRequired = errors.New("reason is required")
	// ErrBadScope: a cutover without a known scope.
	ErrBadScope = errors.New("unknown invalidation scope")
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
	// AttentionUnmappedPrice: a sellable price has no product (or an archived
	// one), so it cannot be bought. It names a price, not a workspace.
	AttentionUnmappedPrice = "unmapped_price"
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
	Complimentary    bool
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
	out := TenantBilling{TenantID: tenantID, BillingRequired: tenant.BillingRequired, Complimentary: tenant.Complimentary, Duplicates: map[string]bool{}}
	if out.Gated, err = s.d.Repo.Gated(ctx, tenantID); err != nil {
		return out, err
	}
	if out.Subscriptions, err = s.d.Repo.ListSubscriptionsByTenant(ctx, nil, tenantID); err != nil {
		return out, err
	}
	followed := s.liveSubscription(out.Subscriptions)
	var priceIDs []string
	for i := range out.Subscriptions {
		sub := &out.Subscriptions[i]
		if followed != nil && sub.ID != followed.ID && s.ours(sub) && billingdom.Live(sub.Status, sub.EndsAt, s.now()) {
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

// AttentionItem is one reason a workspace, or for unmapped_price a price,
// needs an operator.
type AttentionItem struct {
	TenantID       string
	TenantName     string
	Reason         string
	Subscription   *models.BillingSubscription
	WebhookEvent   *models.BillingWebhookEvent
	Since          time.Time
	TeardownStatus string
	// PriceID is set for unmapped_price.
	PriceID string
}

// Attention lists workspaces that need an operator, newest first. The page
// token is an offset; the list is small by nature.
func (s *Service) Attention(ctx context.Context, pageSize int, pageToken string) ([]AttentionItem, string, error) {
	pageSize = min(cmp.Or(pageSize, 50), 200)
	if pageSize <= 0 {
		pageSize = 50
	}
	// The token is operator input: anything that is not a non-negative offset
	// starts from the top.
	offset, err := strconv.Atoi(pageToken)
	if err != nil || offset < 0 {
		offset = 0
	}
	since := s.now().Add(-attentionWindow)
	var items []AttentionItem
	for _, collect := range []func(context.Context, time.Time) ([]AttentionItem, error){
		s.troubledItems, s.expiredItems, s.duplicateItems, s.unresolvedWebhookItems, s.failedChangeItems, s.unmappedPriceItems,
	} {
		found, err := collect(ctx, since)
		if err != nil {
			return nil, "", err
		}
		items = append(items, found...)
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

// troubledItems: subscriptions in dunning or paused.
func (s *Service) troubledItems(ctx context.Context, _ time.Time) ([]AttentionItem, error) {
	subs, err := s.d.Repo.ListByStatus(ctx, s.d.Settings.TestMode, []string{models.SubscriptionStatusPastDue, models.SubscriptionStatusUnpaid, models.SubscriptionStatusPaused}, nil)
	var out []AttentionItem
	for i := range subs {
		out = append(out, AttentionItem{TenantID: subs[i].TenantID, Reason: subs[i].Status, Subscription: &subs[i], Since: subs[i].ProviderUpdatedAt})
	}
	return out, err
}

// expiredItems: subscriptions that ended recently, flagging trials that never
// paid and whether their teardown ran.
func (s *Service) expiredItems(ctx context.Context, since time.Time) ([]AttentionItem, error) {
	subs, err := s.d.Repo.ListByStatus(ctx, s.d.Settings.TestMode, []string{models.SubscriptionStatusExpired}, &since)
	var out []AttentionItem
	for i := range subs {
		sub := &subs[i]
		item := AttentionItem{TenantID: sub.TenantID, Reason: AttentionExpired, Subscription: sub, Since: sub.ProviderUpdatedAt}
		if sub.FirstPaidAt == nil {
			item.Reason, item.TeardownStatus = AttentionExpiredNeverPaid, "dry_run"
			if sub.TeardownAt != nil {
				item.TeardownStatus = "enqueued"
			}
		}
		out = append(out, item)
	}
	return out, err
}

// duplicateItems: every live subscription of a workspace but its newest.
func (s *Service) duplicateItems(ctx context.Context, _ time.Time) ([]AttentionItem, error) {
	live, err := s.d.Repo.ListByStatus(ctx, s.d.Settings.TestMode, []string{models.SubscriptionStatusOnTrial, models.SubscriptionStatusActive, models.SubscriptionStatusPastDue,
		models.SubscriptionStatusUnpaid, models.SubscriptionStatusPaused, models.SubscriptionStatusCancelled}, nil)
	if err != nil {
		return nil, err
	}
	byTenant := map[string][]models.BillingSubscription{}
	for _, sub := range live {
		if billingdom.Live(sub.Status, sub.EndsAt, s.now()) {
			byTenant[sub.TenantID] = append(byTenant[sub.TenantID], sub)
		}
	}
	var out []AttentionItem
	for _, subs := range byTenant {
		slices.SortFunc(subs, func(a, b models.BillingSubscription) int { return b.CreatedAt.Compare(a.CreatedAt) })
		for i := 1; i < len(subs); i++ {
			out = append(out, AttentionItem{TenantID: subs[i].TenantID, Reason: AttentionDuplicate, Subscription: &subs[i], Since: subs[0].CreatedAt})
		}
	}
	return out, nil
}

// unresolvedWebhookItems: failed deliveries whose workspace was not found.
func (s *Service) unresolvedWebhookItems(ctx context.Context, since time.Time) ([]AttentionItem, error) {
	events, err := s.d.Repo.ListFailedWebhookEvents(ctx, since, true)
	var out []AttentionItem
	for i := range events {
		out = append(out, AttentionItem{Reason: AttentionUnresolvedWebhook, WebhookEvent: &events[i], Since: events[i].ReceivedAt})
	}
	return out, err
}

// failedChangeItems: scheduled downgrades that could not be applied.
func (s *Service) failedChangeItems(ctx context.Context, since time.Time) ([]AttentionItem, error) {
	changes, err := s.d.Repo.ListScheduledChangesByStatus(ctx, models.ScheduledChangeFailed, since)
	if err != nil {
		return nil, err
	}
	var out []AttentionItem
	for _, c := range changes {
		sub, err := s.d.Repo.GetSubscription(ctx, c.BillingSubscriptionID)
		if err != nil {
			continue
		}
		out = append(out, AttentionItem{TenantID: c.TenantID, Reason: AttentionScheduledChangeFailed, Subscription: sub, Since: c.UpdatedAt})
	}
	return out, nil
}

// SetBillingRequired comps or exempts a workspace, or puts it back behind the
// gate. A complimentary workspace is refused (ErrComplimentary): ending
// complimentary access is SetComplimentary.
func (s *Service) SetBillingRequired(ctx context.Context, tenantID string, required bool, reason string) error {
	if reason == "" {
		return ErrReasonRequired
	}
	if required {
		comp, err := s.complimentaryTenant(ctx, tenantID)
		if err != nil {
			return err
		}
		if comp {
			return ErrComplimentary
		}
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
// tier's current purchasable version is sold by a product with a one-month
// trial, else 0.
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
	products, err := s.productMap(ctx)
	if err != nil {
		return 0, err
	}
	days, mapped := 0, 0
	for _, r := range rows {
		productID, ok := products.Product(r.ID)
		if !ok {
			continue
		}
		mapped++
		p, err := s.product(ctx, productID)
		if errors.Is(err, billingdom.ErrNotFound) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		if !p.OneMonthTrial() {
			return 0, nil
		}
		days = p.TrialDays()
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
			return FreeTrialOffer{}, ErrTrialProductMissing
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

// InvalidationScope selects the subscriptions a cutover invalidates.
type InvalidationScope int

const (
	// ScopeProviderSwitch: every subscription of a provider other than the
	// one billing runs on (Lemon Squeezy, after the move to Polar).
	ScopeProviderSwitch InvalidationScope = iota + 1
	// ScopeModeCutover: every test-mode subscription, once billing runs live.
	ScopeModeCutover
)

// InvalidateSubscriptions is the cutover: the subscriptions the scope
// selects are invalidated, their open scheduled changes cancelled, their
// workspaces put back behind the billing gate (a complimentary one never is)
// and their owners asked to check out again. The mode cutover is refused
// while still in test mode. A dry run only lists the workspaces; a second
// run finds nothing left to do.
func (s *Service) InvalidateSubscriptions(ctx context.Context, dryRun bool, scope InvalidationScope) ([]string, error) {
	if !s.Enabled() {
		return nil, ErrUnavailable
	}
	var (
		subs   []models.BillingSubscription
		reason string
		err    error
	)
	switch scope {
	case ScopeProviderSwitch:
		reason = models.InvalidatedReasonProviderSwitch
		subs, err = s.d.Repo.ListToInvalidate(ctx, s.provider(), false)
	case ScopeModeCutover:
		if s.d.Settings.TestMode {
			return nil, ErrStillTestMode
		}
		reason = models.InvalidatedReasonModeCutover
		subs, err = s.d.Repo.ListToInvalidate(ctx, "", true)
	default:
		return nil, ErrBadScope
	}
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
		if err := s.invalidateTenant(ctx, tenantID, byTenant[tenantID], reason); err != nil {
			return nil, err
		}
	}
	slog.InfoContext(ctx, "billing: subscriptions invalidated", logging.AttrComponent, "billing",
		"event", "subscriptions_invalidated", "reason", reason, "tenants", len(tenants), "subscriptions", len(subs))
	return tenants, nil
}

// invalidateTenant invalidates one workspace's subscriptions in one
// transaction, then tells its owners.
func (s *Service) invalidateTenant(ctx context.Context, tenantID string, subIDs []string, reason string) error {
	err := s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := s.d.Repo.Invalidate(ctx, tx, subIDs, reason, s.now()); err != nil {
			return err
		}
		for _, id := range subIDs {
			if err := s.cancelOpenChange(ctx, tx, id, "subscription invalidated ("+reason+")"); err != nil {
				return err
			}
		}
		if err := s.d.Repo.RequireBilling(ctx, tx, tenantID, s.now()); err != nil {
			return err
		}
		owners, err := s.d.Users.ListOwnersByTenant(ctx, tenantID)
		if err != nil {
			return err
		}
		for _, o := range owners {
			key := templates.KeyBillingReentryRequired + ":" + reason + ":" + tenantID + ":" + o.ID
			if err := s.d.Jobs.EnqueueBillingEmailTx(ctx, tx.Tx, o.ID, tenantID, templates.KeyBillingReentryRequired, key); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	body := "Our payment provider changed. Choose your plan again to keep going."
	if reason == models.InvalidatedReasonModeCutover {
		body = "Billing moved from its test period to live payments. Choose your plan again to keep going."
	}
	s.emit(ctx, []notice{{tenantID: tenantID, subID: subIDs[0], typ: "reentry_required", episode: reason, level: models.NotificationLevelWarning,
		title: "Please choose your plan again", body: body}})
	s.publish(ctx, tenantID, true)
	return nil
}

// BillingStatus summarises a workspace for the operator tenant list:
// complimentary for a tester's workspace (billing on or off); otherwise empty
// when billing is off; not_required; awaiting_checkout; or the Ogen status
// of its current subscription.
func (s *Service) BillingStatus(ctx context.Context, t *models.Tenant) (string, error) {
	if t.Complimentary {
		return BillingStatusComplimentary, nil
	}
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

// BillingStatusComplimentary is a tester's workspace in the tenant list.
const BillingStatusComplimentary = "complimentary"

// PricingFlags is what the public pricing read adds per tier and price.
type PricingFlags struct {
	Enabled bool
	// MappedPrices are prices with a provider product that is not archived.
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
	products, err := s.productMap(ctx)
	if err != nil {
		return out, err
	}
	for _, id := range products.PriceIDs() {
		productID, _ := products.Product(id)
		// The checkout refuses an archived product; a product that cannot
		// be read now is left on offer, and the checkout decides.
		if p, err := s.product(ctx, productID); err == nil && p.Archived {
			continue
		}
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
