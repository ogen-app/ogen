package billing

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/url"
	"time"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

const (
	checkoutTTL = time.Hour
	portalTTL   = 24 * time.Hour
)

// Buyer is the owner starting a checkout.
type Buyer struct {
	TenantID  string
	UserID    string
	AccountID string
	Email     string
}

// Checkout is a hosted checkout to send the owner to.
type Checkout struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	// Trial reports whether the free month was applied to this checkout.
	Trial bool `json:"trial"`
}

// TrialEligible reports whether an account may still get the free month in
// the current mode.
func (s *Service) TrialEligible(ctx context.Context, accountID string) (bool, error) {
	if accountID == "" {
		return false, nil
	}
	used, err := s.d.Repo.TrialUsed(ctx, accountID, s.d.Settings.TestMode)
	return !used, err
}

// Gated reports whether the billing gate holds a workspace.
func (s *Service) Gated(ctx context.Context, tenantID string) (bool, error) {
	if !s.Enabled() {
		return false, nil
	}
	return s.d.Repo.Gated(ctx, tenantID)
}

// startCheckout opens a hosted checkout for a price. The free month applies
// only to the Harbor-marked tier, for an eligible account, and only when the
// variant really carries a one-month trial; otherwise the trial is skipped and
// the first invoice is charged at checkout.
func (s *Service) startCheckout(ctx context.Context, buyer Buyer, price billingdom.Price, variantID string) (Checkout, error) {
	trial, err := s.trialApplies(ctx, buyer.AccountID, price.TierID, variantID)
	if err != nil {
		return Checkout{}, err
	}
	redirect := s.d.Settings.AppBaseURL + "/plans?billing=return&workspace=" + url.QueryEscape(buyer.TenantID)
	session, err := s.d.Provider.CreateCheckout(ctx, billingdom.CheckoutRequest{
		VariantID: variantID,
		Email:     buyer.Email,
		Custom: map[string]string{
			"tenant_id":  buyer.TenantID,
			"price_id":   price.ID,
			"user_id":    buyer.UserID,
			"account_id": buyer.AccountID,
		},
		RedirectURL: redirect,
		ExpiresAt:   s.now().Add(checkoutTTL),
		TestMode:    s.d.Settings.TestMode,
		SkipTrial:   !trial,
	})
	if err != nil {
		return Checkout{}, providerErr(err)
	}
	checkoutsCreated.Add(1)
	if trial {
		checkoutsTrial.Add(1)
	}
	if s.d.Activity != nil {
		s.d.Activity.Record(tenantctx.With(ctx, buyer.TenantID), activity.CategoryBilling, "checkout_started",
			activity.WithUser(buyer.UserID), activity.WithSource(activity.SourceAPI),
			activity.WithPayload(map[string]any{"price_id": price.ID, "trial": trial}))
	}
	return Checkout{URL: session.URL, ExpiresAt: session.ExpiresAt, Trial: trial}, nil
}

func (s *Service) trialApplies(ctx context.Context, accountID, tierID, variantID string) (bool, error) {
	marked, err := s.d.Repo.FreeTrialTier(ctx)
	if err != nil || marked == nil || marked.ID != tierID {
		return false, err
	}
	eligible, err := s.TrialEligible(ctx, accountID)
	if err != nil || !eligible {
		return false, err
	}
	v, err := s.variant(ctx, variantID)
	if err != nil {
		return false, providerErr(err)
	}
	if !v.OneMonthTrial() {
		freeTrialMisconfigued.Add(1)
		slog.ErrorContext(ctx, "billing: the free-month tier's variant has no one-month trial; charging at checkout", logging.AttrComponent, "billing", "tier_id", tierID, "variant_id", variantID)
		return false, nil
	}
	return true, nil
}

// SubscriptionView is the owner's billing card. Status is the provider's own
// vocabulary.
type SubscriptionView struct {
	Status   string     `json:"status"`
	RenewsAt *time.Time `json:"renews_at"`
	EndsAt   *time.Time `json:"ends_at"`
	Card     *CardView  `json:"card"`
	Price    *PriceView `json:"price"`
}

// CardView is the card brand and last four digits.
type CardView struct {
	Brand    string `json:"brand"`
	LastFour string `json:"last_four"`
}

// PriceView is a net (VAT-exclusive) price.
type PriceView struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Period   string `json:"period"`
}

// BillingView is GET /api/billing.
type BillingView struct {
	// Subscription is nil when the workspace never bought.
	Subscription *SubscriptionView `json:"subscription"`
	// Portal reports whether there is a customer portal to send the owner to.
	Portal   bool `json:"portal"`
	TestMode bool `json:"test_mode"`
}

// Billing builds the owner's billing card.
func (s *Service) Billing(ctx context.Context, tenantID string) (BillingView, error) {
	if !s.Enabled() {
		return BillingView{}, ErrUnavailable
	}
	view := BillingView{TestMode: s.d.Settings.TestMode}
	subs, err := s.d.Repo.ListSubscriptionsByTenant(ctx, nil, tenantID)
	if err != nil {
		return view, err
	}
	sub := s.currentSubscription(subs)
	if sub == nil {
		return view, nil
	}
	view.Portal = true
	sv := &SubscriptionView{Status: sub.Status, RenewsAt: sub.RenewsAt, EndsAt: sub.EndsAt}
	if sub.Status == models.SubscriptionStatusCancelled || sub.Status == models.SubscriptionStatusExpired {
		sv.RenewsAt = nil
	}
	if sub.CardLastFour != "" {
		sv.Card = &CardView{Brand: sub.CardBrand, LastFour: sub.CardLastFour}
	}
	if sub.TierVersionPriceID != nil {
		prices, err := s.d.Repo.Prices(ctx, []string{*sub.TierVersionPriceID})
		if err != nil {
			return view, err
		}
		if p, ok := prices[*sub.TierVersionPriceID]; ok {
			sv.Price = &PriceView{Amount: p.NetMinor, Currency: p.Currency, Period: p.Interval}
		}
	}
	view.Subscription = sv
	return view, nil
}

// Portal returns a fresh customer-portal link for the workspace's
// subscription. The link is signed and short-lived; never log or cache it.
func (s *Service) Portal(ctx context.Context, tenantID string) (string, time.Time, error) {
	if !s.Enabled() {
		return "", time.Time{}, ErrUnavailable
	}
	subs, err := s.d.Repo.ListSubscriptionsByTenant(ctx, nil, tenantID)
	if err != nil {
		return "", time.Time{}, err
	}
	sub := s.currentSubscription(subs)
	if sub == nil {
		return "", time.Time{}, ErrNoSubscription
	}
	snap, err := s.d.Provider.GetSubscription(ctx, sub.ProviderSubscriptionID)
	if errors.Is(err, billingdom.ErrNotFound) {
		return "", time.Time{}, ErrNoSubscription
	}
	if err != nil {
		return "", time.Time{}, providerErr(err)
	}
	if snap.PortalURL == "" {
		return "", time.Time{}, ErrNoSubscription
	}
	return snap.PortalURL, s.now().Add(portalTTL), nil
}

// tenantExists checks a live workspace.
func (s *Service) tenantExists(ctx context.Context, tenantID string) error {
	t, err := s.d.Tenants.GetByID(ctx, tenantID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && t.DeletedAt != nil) {
		return ErrTenantNotFound
	}
	return err
}
