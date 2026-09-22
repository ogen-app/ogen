package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/usecase/campaign_actions/overview"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/summaries"
)

// CampaignReadHandler owns the campaign read/projection endpoints — the
// per-campaign overview (GET /:id/overview, CON-113) and the batched
// Campaigns-list summaries (GET /summaries, CON-152). Split out of the
// CampaignsHandler god-object (CON-291): a focused handler over the two plain
// tenant-scoped DB-read services, each nil-disabling its endpoint with a 503.
//
// It must be registered BEFORE CampaignsHandler so the static /summaries route
// is matched ahead of the dynamic /:id route.
type CampaignReadHandler struct {
	overview  *overview.Service
	summaries *summaries.Service
	auth      fiber.Handler
}

// NewCampaignReadHandler wires the campaign read endpoints. overview / summaries
// are optional — a nil one leaves its endpoint at 503.
func NewCampaignReadHandler(overviewSvc *overview.Service, summariesSvc *summaries.Service, auth fiber.Handler) *CampaignReadHandler {
	return &CampaignReadHandler{overview: overviewSvc, summaries: summariesSvc, auth: auth}
}

// Register mounts the routes. /summaries is registered before any /:id route
// (see the CampaignsHandler ordering note) so it is not shadowed.
func (h *CampaignReadHandler) Register(app *fiber.App) {
	app.Get("/api/campaigns/summaries", h.auth, h.Summaries)
	app.Get("/api/campaigns/:id/overview", h.auth, h.Overview)
}

// Overview godoc
// @Summary      Campaign overview
// @Description  Returns the per-campaign overview projection (CON-113): a plain
// @Description  tenant-scoped DB read, available to any user in the tenant.
// @Tags         campaigns
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Campaign Sqid"
// @Success      200  {object}  overview.Overview
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/campaigns/{id}/overview [get]
func (h *CampaignReadHandler) Overview(c *fiber.Ctx) error {
	if h.overview == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "campaign overview is not available")
	}
	ov, err := h.overview.Overview(reqCtx(c), c.Params("id"))
	if err != nil {
		if errors.Is(err, overview.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "campaign not found")
		}
		return err
	}
	return c.JSON(ov)
}

// Summaries godoc
// @Summary      Batched campaign post summaries
// @Description  Returns a slim per-post projection for every campaign in the
// @Description  tenant, grouped by campaign (CON-152). The Campaigns list runs
// @Description  its readiness rules on this single response instead of firing
// @Description  one GET /campaigns/:id/posts per card. Carries only the fields
// @Description  the rules read — no title, content, or hydrated relations.
// @Tags         campaigns
// @Produce      json
// @Security     CookieAuth
// @Success      200  {object}  summaries.Summaries
// @Failure      401  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/campaigns/summaries [get]
func (h *CampaignReadHandler) Summaries(c *fiber.Ctx) error {
	if h.summaries == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "campaign summaries are not available")
	}
	out, err := h.summaries.Summaries(reqCtx(c))
	if err != nil {
		return err
	}
	return c.JSON(out)
}
