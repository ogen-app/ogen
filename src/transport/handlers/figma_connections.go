package handlers

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/plugins"
)

// FigmaConnectionsHandler is the web app's side of the Figma plugin: the
// approval page for a pairing, and the member's plugin connections. Cookie
// authenticated; the approval binds the token to the request's active
// workspace (X-Workspace-Id).
type FigmaConnectionsHandler struct {
	svc      *plugins.Service
	users    repository.UserRepository
	auth     fiber.Handler
	activity *activity.Recorder
}

// NewFigmaConnectionsHandler builds the handler. A nil rec records nothing.
func NewFigmaConnectionsHandler(svc *plugins.Service, users repository.UserRepository, auth fiber.Handler, rec *activity.Recorder) *FigmaConnectionsHandler {
	return &FigmaConnectionsHandler{svc: svc, users: users, auth: auth, activity: rec}
}

func (h *FigmaConnectionsHandler) Register(app *fiber.App) {
	g := app.Group("/api/integrations/figma", h.auth)
	g.Get("/pairings/:write_key", h.PreviewPairing)
	g.Post("/pairings/:write_key/approve", h.ApprovePairing)
	g.Post("/pairings/:write_key/deny", h.DenyPairing)
	g.Get("/connections", h.ListConnections)
	g.Delete("/connections/:id", h.DeleteConnection)
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

type connectionsResponse struct {
	Connections []pluginConnectionResponse `json:"connections"`
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

// ListConnections godoc
// @Summary     List Figma plugin connections
// @Description Live connections in the active workspace, newest first: a member sees their own, an owner sees everyone's. last_used_at is updated at most every 10 minutes.
// @Tags        plugins
// @Produce     json
// @Security    CookieAuth
// @Success     200 {object} connectionsResponse
// @Failure     401 {object} map[string]string
// @Router      /api/integrations/figma/connections [get]
func (h *FigmaConnectionsHandler) ListConnections(c *fiber.Ctx) error {
	caller, err := callerUser(c, h.users)
	if err != nil {
		return err
	}
	rows, err := h.svc.ListConnections(reqCtx(c), caller.TenantID, caller)
	if err != nil {
		return err
	}
	out := connectionsResponse{Connections: make([]pluginConnectionResponse, 0, len(rows))}
	for i := range rows {
		r := connectionResponse(&rows[i].PluginToken)
		r.User = &pluginUser{ID: rows[i].UserID, Name: rows[i].UserName, Email: rows[i].UserEmail}
		out.Connections = append(out.Connections, r)
	}
	return c.JSON(out)
}

// DeleteConnection godoc
// @Summary     Disconnect a Figma plugin
// @Description Revokes the connection; the plugin's next call gets 401 plugin_token_invalid. Members may disconnect their own connections, owners any in the workspace.
// @Tags        plugins
// @Security    CookieAuth
// @Param       id path string true "connection id"
// @Success     204
// @Failure     401 {object} map[string]string
// @Failure     403 {object} map[string]string
// @Failure     404 {object} map[string]string
// @Router      /api/integrations/figma/connections/{id} [delete]
func (h *FigmaConnectionsHandler) DeleteConnection(c *fiber.Ctx) error {
	caller, err := callerUser(c, h.users)
	if err != nil {
		return err
	}
	tok, err := h.svc.Disconnect(reqCtx(c), caller.TenantID, caller, c.Params("id"))
	switch {
	case errors.Is(err, plugins.ErrConnectionNotFound):
		return fiber.NewError(fiber.StatusNotFound, err.Error())
	case errors.Is(err, plugins.ErrConnectionForbidden):
		return fiber.NewError(fiber.StatusForbidden, err.Error())
	case err != nil:
		return err
	}
	h.recordActivity(c, "plugin_revoked", tok)
	return c.SendStatus(fiber.StatusNoContent)
}
