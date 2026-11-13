package handlers

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
)

// CampaignAssistantStateHandler serves what the campaign assistant knows of
// a campaign before it is asked.
type CampaignAssistantStateHandler struct {
	read func(ctx context.Context, campaignID string) (*campaign_assistant.CampaignState, error)
	auth fiber.Handler
}

// NewCampaignAssistantStateHandler wires GET /api/campaigns/:id/assistant-state.
func NewCampaignAssistantStateHandler(read func(ctx context.Context, campaignID string) (*campaign_assistant.CampaignState, error), auth fiber.Handler) *CampaignAssistantStateHandler {
	return &CampaignAssistantStateHandler{read: read, auth: auth}
}

// Register mounts the route.
func (h *CampaignAssistantStateHandler) Register(app *fiber.App) {
	app.Get("/api/campaigns/:id/assistant-state", h.auth, h.State)
}

// State godoc
// @Summary      Campaign assistant state
// @Description  The snapshot the campaign assistant reads at the top of every campaign turn — the goal and the
// @Description  current goal period's slots left, posts by status, platforms, the calendar's problems, and the
// @Description  sources attached, analysed and ready, each attached source with its status — and the starters
// @Description  that lead with those numbers. Every campaign turn's results.campaign carries the same state and
// @Description  starters; this read serves the panel before the first turn. No model call.
// @Tags         assistant
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Campaign Sqid"
// @Success      200  {object}  campaign_assistant.CampaignState
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/campaigns/{id}/assistant-state [get]
func (h *CampaignAssistantStateHandler) State(c *fiber.Ctx) error {
	st, err := h.read(reqCtx(c), c.Params("id"))
	if errors.Is(err, sql.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "campaign not found")
	}
	if err != nil {
		return err
	}
	return c.JSON(st)
}
