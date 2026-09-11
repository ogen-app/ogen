package handlers

import (
	"cmp"
	"math"
	"slices"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// PricingHandler serves CON-243's read surface: the unauthenticated marketing
// pricing catalog and the authenticated in-app entitlement view.
type PricingHandler struct {
	resolver *entitlements.Resolver
	versions repository.TenantTierVersionRepository
	catalog  *entitlements.Catalog
	auth     fiber.Handler
}

// NewPricingHandler builds the handler over the entitlement resolver, the
// version repository (for the public catalog), the feature catalog, and the auth
// middleware.
func NewPricingHandler(resolver *entitlements.Resolver, versions repository.TenantTierVersionRepository, catalog *entitlements.Catalog, auth fiber.Handler) *PricingHandler {
	return &PricingHandler{resolver: resolver, versions: versions, catalog: catalog, auth: auth}
}

func (h *PricingHandler) Register(app *fiber.App) {
	app.Get("/api/public/pricing", h.Pricing)                 // PUBLIC — no auth (marketing site)
	app.Get("/api/me/entitlements", h.auth, h.MyEntitlements) // authenticated
}

// pricingResponse wraps the ordered tiers so the envelope can grow (e.g. an
// applied VAT rate) without a breaking change.
type pricingResponse struct {
	Tiers []entitlements.Resolution `json:"tiers"`
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

	versions, err := h.versions.ListCurrentPurchasable(c.Context())
	if err != nil {
		return err
	}
	ids := make([]string, len(versions))
	for i := range versions {
		ids[i] = versions[i].ID
	}
	pricesByVersion, err := h.versions.PricesByVersionIDs(c.Context(), ids)
	if err != nil {
		return err
	}

	tiers := make([]entitlements.Resolution, 0, len(versions))
	for i := range versions {
		v := &versions[i]
		tiers = append(tiers, entitlements.Enrich(h.catalog, v, pricesByVersion[v.ID]))
	}
	// Cheapest monthly net price first (Trial → Pro → Max); price-less tiers last,
	// then by tier id for a stable order.
	slices.SortStableFunc(tiers, func(a, b entitlements.Resolution) int {
		return cmp.Or(cmp.Compare(minMonthlyNet(a), minMonthlyNet(b)), cmp.Compare(a.TierID, b.TierID))
	})

	return c.JSON(pricingResponse{Tiers: tiers})
}

// MyEntitlements godoc
// @Summary      Current entitlements
// @Description  The authenticated tenant's tier version in force now: its price, its full entitlement set, and its change_reason (CON-243). The cheapest answer to a disputed change — the customer sees exactly what they are on.
// @Tags         pricing
// @Produce      json
// @Security     CookieAuth
// @Success      200  {object}  entitlements.Resolution
// @Failure      401  {object}  map[string]string
// @Router       /api/me/entitlements [get]
func (h *PricingHandler) MyEntitlements(c *fiber.Ctx) error {
	tenantID, ok := tenantctx.From(c.Context())
	if !ok {
		if s, ok := c.Locals("session").(*models.Session); ok && s != nil {
			tenantID = s.TenantID
		}
	}
	if tenantID == "" {
		return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	res, err := h.resolver.ResolveCurrent(c.Context(), tenantID)
	if err != nil {
		return notFound(err, "no entitlements resolved for this workspace")
	}
	return c.JSON(res)
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
