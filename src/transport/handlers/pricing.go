package handlers

import (
	"cmp"
	"context"
	"math"
	"slices"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/billing"
)

// PricingHandler serves CON-243's read surface: the unauthenticated marketing
// pricing catalog and the authenticated in-app entitlement view.
type PricingHandler struct {
	resolver *entitlements.Resolver
	versions repository.TenantTierVersionRepository
	catalog  *entitlements.Catalog
	auth     fiber.Handler
	limiter  *entitlements.Limiter // Live usage counts for /me/entitlements (nil-safe)
	billing  BillingPlanReader     // Billing facts for both reads (nil = billing off)
}

// BillingPlanReader is the billing half of the plan reads: workspace billing
// facts for the entitlements read, purchasability for public pricing.
type BillingPlanReader interface {
	PlanState(ctx context.Context, tenantID, accountID string) (billing.PlanState, error)
	PricingFlags(ctx context.Context) (billing.PricingFlags, error)
}

// WithBilling adds the billing facts to both reads.
func (h *PricingHandler) WithBilling(b BillingPlanReader) *PricingHandler {
	h.billing = b
	return h
}

// NewPricingHandler builds the handler over the entitlement resolver, the
// version repository (for the public catalog), the feature catalog, and the auth
// middleware.
//
// limiter lets MyEntitlements attach each numeric feature's live usage; nil
// makes the read omit `current`.
func NewPricingHandler(resolver *entitlements.Resolver, versions repository.TenantTierVersionRepository, catalog *entitlements.Catalog, auth fiber.Handler, limiter *entitlements.Limiter) *PricingHandler {
	return &PricingHandler{resolver: resolver, versions: versions, catalog: catalog, auth: auth, limiter: limiter}
}

func (h *PricingHandler) Register(app *fiber.App) {
	app.Get("/api/public/pricing", h.Pricing)                 // PUBLIC — no auth (marketing site)
	app.Get("/api/me/entitlements", h.auth, h.MyEntitlements) // authenticated
}

// pricingResponse wraps the ordered tiers so the envelope can grow (e.g. an
// applied VAT rate) without a breaking change.
type pricingResponse struct {
	Tiers []pricingTier `json:"tiers"`
}

// pricingTier is a Resolution plus billing purchasability. Its Prices shadow
// the embedded ones to carry a per-price available flag.
type pricingTier struct {
	entitlements.Resolution
	// Available: the tier can be bought now (billing on and at least one of its
	// prices sold by a provider variant).
	Available bool `json:"available"`
	// FreeTrial marks the tier carrying the marketing free month; TrialDays is
	// its length (null without the offer).
	FreeTrial bool           `json:"free_trial"`
	TrialDays *int           `json:"trial_days" extensions:"x-nullable"`
	Prices    []pricingPrice `json:"prices"`
}

type pricingPrice struct {
	models.TenantTierVersionPrice
	Available bool `json:"available"`
}

// myEntitlementsResponse is the Resolution plus the workspace's billing facts.
type myEntitlementsResponse struct {
	*entitlements.Resolution
	billing.PlanState
}

// Pricing godoc
// @Summary      Public pricing catalog
// @Description  Unauthenticated source for the marketing Pricing page (CON-243). Returns every currently offered tier — each tier's latest active + purchasable version — with its price rows and its entitlement values enriched with catalog metadata. Net prices; retired/draft/internal tiers never appear. Cacheable (versions are immutable).
// @Tags         pricing
// @Produce      json
// @Success      200  {object}  pricingResponse
// @Router       /api/public/pricing [get]
func (h *PricingHandler) Pricing(c *fiber.Ctx) error {
	// Public, non-credentialed, CDN-friendly read.
	c.Set("Access-Control-Allow-Origin", "*")
	c.Set("Cache-Control", "public, max-age=300")

	versions, err := h.versions.ListCurrentPurchasable(reqCtx(c))
	if err != nil {
		return err
	}
	ids := make([]string, len(versions))
	for i := range versions {
		ids[i] = versions[i].ID
	}
	pricesByVersion, err := h.versions.PricesByVersionIDs(reqCtx(c), ids)
	if err != nil {
		return err
	}

	var flags billing.PricingFlags
	if h.billing != nil {
		if flags, err = h.billing.PricingFlags(reqCtx(c)); err != nil {
			return err
		}
	}
	tiers := make([]pricingTier, 0, len(versions))
	for i := range versions {
		v := &versions[i]
		res := entitlements.Enrich(h.catalog, v, pricesByVersion[v.ID])
		tier := pricingTier{Resolution: res, Prices: make([]pricingPrice, 0, len(res.Prices))}
		for _, p := range res.Prices {
			mapped := flags.Enabled && flags.MappedPrices[p.ID]
			tier.Available = tier.Available || mapped
			tier.Prices = append(tier.Prices, pricingPrice{TenantTierVersionPrice: p, Available: mapped})
		}
		if flags.FreeTrialTierID != "" && flags.FreeTrialTierID == res.TierID {
			tier.FreeTrial, tier.TrialDays = true, new(flags.TrialDays)
		}
		tiers = append(tiers, tier)
	}
	// Cheapest monthly net price first (Trial → Pro → Max); price-less tiers last,
	// then by tier id for a stable order.
	slices.SortStableFunc(tiers, func(a, b pricingTier) int {
		return cmp.Or(cmp.Compare(minMonthlyNet(a.Resolution), minMonthlyNet(b.Resolution)), cmp.Compare(a.TierID, b.TierID))
	})

	return c.JSON(pricingResponse{Tiers: tiers})
}

// MyEntitlements godoc
// @Summary      Current entitlements
// @Description  The authenticated tenant's tier version in force now: its price, its full entitlement set, and its change_reason (CON-243). The cheapest answer to a disputed change — the customer sees exactly what they are on.
// @Tags         pricing
// @Produce      json
// @Security     CookieAuth
// @Success      200  {object}  myEntitlementsResponse
// @Failure      401  {object}  map[string]string
// @Router       /api/me/entitlements [get]
func (h *PricingHandler) MyEntitlements(c *fiber.Ctx) error {
	tenantID, ok := tenantctx.From(reqCtx(c))
	var accountID string
	if s, ok := c.Locals("session").(*models.Session); ok && s != nil {
		accountID = s.AccountID
		if tenantID == "" {
			tenantID = s.TenantID
		}
	}
	if !ok && tenantID == "" {
		return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	body, err := h.entitlementsBody(c, tenantID, accountID)
	if err != nil {
		return err
	}
	return c.JSON(body)
}

// entitlementsBody is the GET /api/me/entitlements body; a 200 plan change
// answers with it too.
func (h *PricingHandler) entitlementsBody(c *fiber.Ctx, tenantID, accountID string) (any, error) {
	res, err := h.resolver.ResolveCurrent(reqCtx(c), tenantID)
	if err != nil {
		return nil, notFound(err, "no entitlements resolved for this workspace")
	}
	// Attach live usage to each numeric feature that has a registered
	// counter, so the client can render "N of M" and warn before the 402 rather
	// than learning the cap only on refusal. Uncounted / boolean features keep
	// current=nil (omitted). Scope the context to the resolved tenant so the
	// counters (which read the tenant from ctx) count the right workspace.
	qctx := tenantctx.With(reqCtx(c), tenantID)
	for i := range res.Entitlements {
		if res.Entitlements[i].ValueType != entitlements.ValueTypeNumeric {
			continue
		}
		if n, ok := h.limiter.CurrentUsage(qctx, tenantID, res.Entitlements[i].Key); ok {
			res.Entitlements[i].Current = &n
		}
	}
	out := myEntitlementsResponse{Resolution: res}
	if h.billing != nil {
		if out.PlanState, err = h.billing.PlanState(reqCtx(c), tenantID, accountID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// minMonthlyNet returns the lowest monthly net price of a tier, or math.MaxInt64
// when it has no monthly price (so price-less tiers sort last).
func minMonthlyNet(r entitlements.Resolution) int64 {
	best := int64(math.MaxInt64)
	for _, p := range r.Prices {
		if p.BillingInterval == "month" {
			best = min(best, p.NetMinor)
		}
	}
	return best
}
