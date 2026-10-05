package handlers

import (
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/plugins"
)

// FigmaConnectionsHandler is the web app's side of the Figma plugin: the
// approval page for a pairing, and the member's plugin connections. Cookie
// authenticated; the approval binds the token to the request's active
// workspace (X-Workspace-Id).
type FigmaConnectionsHandler struct {
	svc      *plugins.Service
	auth     fiber.Handler
	activity *activity.Recorder
}

// NewFigmaConnectionsHandler builds the handler. A nil rec records nothing.
func NewFigmaConnectionsHandler(svc *plugins.Service, auth fiber.Handler, rec *activity.Recorder) *FigmaConnectionsHandler {
	return &FigmaConnectionsHandler{svc: svc, auth: auth, activity: rec}
}

func (h *FigmaConnectionsHandler) Register(app *fiber.App) {
	g := app.Group("/api/integrations/figma", h.auth)
	g.Get("/pairings/:write_key", h.PreviewPairing)
	g.Post("/pairings/:write_key/approve", h.ApprovePairing)
	g.Post("/pairings/:write_key/deny", h.DenyPairing)
}

func (h *FigmaConnectionsHandler) recordActivity(c *fiber.Ctx, typ string, tok *models.PluginToken) {
	if h.activity == nil {
		return
	}
	h.activity.Record(reqCtx(c), activity.CategoryIntegration, typ,
		activity.WithSource(activity.SourceAPI),
		activity.WithEntity("plugin_connection", tok.ID),
		activity.WithPayload(map[string]any{"client": tok.Client, "label": tok.Label}),
	)
}

type pairingPreviewResponse struct {
	Client      string    `json:"client"`
	ClientLabel string    `json:"client_label"`
	Status      string    `json:"status"`
	CreatedIP   string    `json:"created_ip"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type pluginConnectionResponse struct {
	ID         string      `json:"id"`
	Client     string      `json:"client"`
	Label      string      `json:"label"`
	User       *pluginUser `json:"user,omitempty"`
	CreatedAt  time.Time   `json:"created_at"`
	LastUsedAt *time.Time  `json:"last_used_at"`
}

type approvePairingResponse struct {
	Connection pluginConnectionResponse `json:"connection"`
}

func connectionResponse(t *models.PluginToken) pluginConnectionResponse {
	return pluginConnectionResponse{
		ID: t.ID, Client: t.Client, Label: t.Label, CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt,
	}
}

// PreviewPairing godoc
// @Summary     Show a plugin pairing awaiting approval
// @Description What the approval page shows before the user allows or denies the plugin. 410 pairing_expired for an expired, unknown or already-collected key.
// @Tags        plugins
// @Produce     json
// @Security    CookieAuth
// @Param       write_key path string true "write key from the approve_url"
// @Success     200 {object} pairingPreviewResponse
// @Failure     401 {object} map[string]string
// @Failure     410 {object} map[string]string
// @Router      /api/integrations/figma/pairings/{write_key} [get]
func (h *FigmaConnectionsHandler) PreviewPairing(c *fiber.Ctx) error {
	p, err := h.svc.Preview(reqCtx(c), c.Params("write_key"))
	if err != nil {
		return pairingError(c, err)
	}
	return c.JSON(pairingPreviewResponse{
		Client: p.Client, ClientLabel: p.ClientLabel, Status: p.Status,
		CreatedIP: p.CreatedIP, CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt,
	})
}

// ApprovePairing godoc
// @Summary     Allow a plugin to act for you in this workspace
// @Description Creates the plugin connection in the active workspace (X-Workspace-Id) for the signed-in member; the plugin collects its token on its next poll. 409 pairing_not_pending when the request was already answered.
// @Tags        plugins
// @Produce     json
// @Security    CookieAuth
// @Param       write_key path string true "write key from the approve_url"
// @Success     200 {object} approvePairingResponse
// @Failure     401 {object} map[string]string
// @Failure     409 {object} map[string]string
// @Failure     410 {object} map[string]string
// @Router      /api/integrations/figma/pairings/{write_key}/approve [post]
func (h *FigmaConnectionsHandler) ApprovePairing(c *fiber.Ctx) error {
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	tok, err := h.svc.Approve(reqCtx(c), c.Params("write_key"), session)
	if err != nil {
		return pairingError(c, err)
	}
	h.recordActivity(c, "plugin_connected", tok)
	return c.JSON(approvePairingResponse{Connection: connectionResponse(tok)})
}

// DenyPairing godoc
// @Summary     Refuse a plugin pairing
// @Tags        plugins
// @Security    CookieAuth
// @Param       write_key path string true "write key from the approve_url"
// @Success     204
// @Failure     401 {object} map[string]string
// @Failure     409 {object} map[string]string
// @Failure     410 {object} map[string]string
// @Router      /api/integrations/figma/pairings/{write_key}/deny [post]
func (h *FigmaConnectionsHandler) DenyPairing(c *fiber.Ctx) error {
	if err := h.svc.Deny(reqCtx(c), c.Params("write_key")); err != nil {
		return pairingError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
