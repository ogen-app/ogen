package handlers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/crypto/envelope"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// This file implements the CON-217 headless connect flow: the browser-facing
// OAuth callback that drives target selection server-side, and the in-Ogen
// picker endpoints for the multi-target case. The connect-link mint lives in
// CreateConnectLink (zernio.go); everything after the user authorizes lands
// here.

// connectSecrets is the sealed payload persisted while a connect awaits an
// in-Ogen pick. These are Zernio's short-lived secrets — never returned to the
// browser, never logged.
type connectSecrets struct {
	ConnectToken string          `json:"connectToken"`
	TempToken    string          `json:"tempToken"`
	UserProfile  json.RawMessage `json:"userProfile,omitempty"`
}

// ConnectCallback is Zernio's headless post-OAuth redirect target (CON-217).
// It is unauthenticated at the cookie layer — the browser may drop the session
// cookie across the cross-site redirect — and instead authenticates via the
// unguessable connect-session id (ogen_cn), from which it resolves the tenant.
//
// Outcomes (all end in a 302 to the SPA):
//   - non-selection platform / already finalized → finalize, success landing
//   - selection platform, exactly one target     → auto-select, success landing
//   - selection platform, 2+ targets             → stash + picker landing
//   - stale/expired/mismatch/upstream failure     → error landing
//
// ConnectCallback godoc
// @Summary      Zernio headless OAuth callback
// @Description  Browser redirect target from Zernio after the user authorizes a
// @Description  platform (headless mode). Authenticated by the opaque connect
// @Description  session id (ogen_cn), not a cookie. Drives any secondary target
// @Description  selection server-side, then 302s to the SPA (success or picker).
// @Tags         zernio
// @Param        ogen_cn  query  string  true  "Connect session id"
// @Success      302  "Redirect to SPA success or picker page"
// @Router       /api/integrations/zernio/connect/callback [get]
func (h *ZernioHandler) ConnectCallback(c *fiber.Ctx) error {
	jobs.ZernioConnectCallbackReceived.Add(1)

	cn := c.Query("ogen_cn")
	if cn == "" {
		return h.redirectConnectError(c, "", "missing_session")
	}
	now := time.Now().UTC()
	sess, err := h.connectSessions.GetLive(c.Context(), cn, now)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jobs.ZernioConnectSessionExpired.Add(1)
			return h.redirectConnectError(c, "", "expired")
		}
		return err
	}

	// Resolve the tenant FROM the row: the browser round-tripped through Zernio
	// without our session, so the opaque id is the capability that re-scopes us.
	ctx := tenantctx.With(c.Context(), sess.TenantID)

	// Defense in depth: the profile Zernio echoes back must match the session's.
	if pid := c.Query("profileId"); pid != "" && pid != sess.ProfileID {
		slog.WarnContext(ctx, "connect callback profile mismatch",
			logging.AttrComponent, "zernio", "connectionId", cn, "platform", sess.Platform)
		_ = h.connectSessions.Delete(ctx, cn)
		return h.redirectConnectError(c, sess.Platform, "mismatch")
	}

	// Already consumed (double redirect / retry). If the session is still
	// awaiting an in-Ogen pick, send the user back to the picker — the account
	// is NOT attached until they choose, so a "connected" landing would be a
	// lie and would strand them away from the selection. Any other non-pending
	// status is a replayed finished step → idempotent success landing.
	if sess.Status == models.ZernioConnectStatusAwaitingSelection {
		return h.redirectPicker(c, cn)
	}
	if sess.Status != models.ZernioConnectStatusPendingAuth {
		return h.redirectConnectSuccess(c, sess.Platform)
	}

	step := c.Query("step")
	if step == "" || !zernio.SupportsConnectSelect(sess.Platform) {
		// No secondary selection — the account was created upstream during OAuth.
		_ = h.connectSessions.Delete(ctx, cn)
		h.finalizeConnect(ctx)
		jobs.ZernioConnectAutoFinalized.Add(1)
		slog.InfoContext(ctx, "connect finalized (no selection)",
			logging.AttrComponent, "zernio", "platform", sess.Platform, "profile", sess.ProfileID)
		return h.redirectConnectSuccess(c, sess.Platform)
	}

	// Selection platform: list the targets and drive the pick ourselves so no
	// Zernio screen ever appears.
	tempToken := c.Query("tempToken")
	connectToken := c.Query("connect_token")
	userProfile := rawUserProfile(c.Query("userProfile"))

	targets, err := h.integ.Client.ListConnectTargets(ctx, sess.Platform, sess.ProfileID, tempToken, connectToken)
	if err != nil {
		if apiErr, ok := errors.AsType[*zernio.APIError](err); ok {
			code := connectErrorCodeForStatus(apiErr.Status)
			slog.WarnContext(ctx, "connect list targets failed",
				logging.AttrComponent, "zernio", "platform", sess.Platform,
				"status", apiErr.Status, "reason", apiErr.Message, "code", code)
			return h.redirectConnectError(c, sess.Platform, code)
		}
		return err
	}

	switch {
	case len(targets) == 0:
		_ = h.connectSessions.Delete(ctx, cn)
		return h.redirectConnectError(c, sess.Platform, "no_targets")

	case len(targets) == 1:
		if err := h.integ.Client.SelectConnectTarget(ctx, sess.Platform, sess.ProfileID, tempToken, connectToken, targets[0].ID, userProfile); err != nil {
			jobs.ZernioConnectSelectFailed.Add(1)
			if apiErr, ok := errors.AsType[*zernio.APIError](err); ok {
				code := connectErrorCodeForStatus(apiErr.Status)
				slog.WarnContext(ctx, "connect select target failed (single target)",
					logging.AttrComponent, "zernio", "platform", sess.Platform,
					"status", apiErr.Status, "reason", apiErr.Message, "code", code)
				return h.redirectConnectError(c, sess.Platform, code)
			}
			return err
		}
		_ = h.connectSessions.Delete(ctx, cn)
		h.finalizeConnect(ctx)
		jobs.ZernioConnectAutoFinalized.Add(1)
		jobs.ZernioConnectSelectSucceeded.Add(1)
		slog.InfoContext(ctx, "connect finalized (single target)",
			logging.AttrComponent, "zernio", "platform", sess.Platform, "profile", sess.ProfileID)
		return h.redirectConnectSuccess(c, sess.Platform)

	default:
		sealed, err := h.sealSecrets(connectSecrets{ConnectToken: connectToken, TempToken: tempToken, UserProfile: userProfile})
		if err != nil {
			return err
		}
		optsJSON, err := json.Marshal(targets)
		if err != nil {
			return err
		}
		sess.Status = models.ZernioConnectStatusAwaitingSelection
		sess.SealedSecrets = sealed
		sess.Options = optsJSON
		if err := h.connectSessions.Update(ctx, sess); err != nil {
			return err
		}
		jobs.ZernioConnectSelectorShown.Add(1)
		slog.InfoContext(ctx, "connect awaiting selection",
			logging.AttrComponent, "zernio", "platform", sess.Platform, "connectionId", cn, "targets", len(targets))
		return h.redirectPicker(c, cn)
	}
}

type pendingConnectionResponse struct {
	Platform string                 `json:"platform"`
	Options  []zernio.ConnectTarget `json:"options"`
}

// GetPendingConnection returns the selectable targets for an awaiting-selection
// connect session, scoped to the caller's tenant (CON-217). It never returns
// the sealed Zernio tokens.
//
// GetPendingConnection godoc
// @Summary      List a pending connect's selectable targets
// @Description  Backs the in-Ogen picker for a multi-target connect (LinkedIn
// @Description  orgs, Facebook pages). Returns display fields only.
// @Tags         zernio
// @Produce      json
// @Security     CookieAuth
// @Param        id   path   string  true  "Connect session id"
// @Success      200  {object}  pendingConnectionResponse
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string  "connection_not_found"
// @Failure      409  {object}  map[string]string  "integration_disabled"
// @Router       /api/integrations/zernio/connect/pending/{id} [get]
func (h *ZernioHandler) GetPendingConnection(c *fiber.Ctx) error {
	if !h.integ.Enabled() {
		return fiber.NewError(fiber.StatusConflict, "integration_disabled")
	}
	sess, err := h.loadPendingForTenant(c)
	if err != nil {
		return err
	}
	var opts []zernio.ConnectTarget
	if len(sess.Options) > 0 {
		if err := json.Unmarshal(sess.Options, &opts); err != nil {
			return err
		}
	}
	return c.JSON(pendingConnectionResponse{Platform: sess.Platform, Options: opts})
}

type selectConnectionRequest struct {
	TargetID string `json:"targetId" validate:"required"`
}

// SelectPendingConnection finalizes a multi-target connect by attaching the
// chosen target (CON-217). It decrypts the stored Zernio tokens server-side,
// calls Zernio's select endpoint, then deletes the session and nudges the sync
// worker so the account mirrors quickly.
//
// SelectPendingConnection godoc
// @Summary      Finalize a pending connect by selecting a target
// @Description  Attaches the chosen target (page/org) to the tenant's profile.
// @Tags         zernio
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path   string                    true  "Connect session id"
// @Param        body  body   selectConnectionRequest   true  "Chosen target id"
// @Success      200  {object}  map[string]string
// @Failure      400  {object}  map[string]string  "invalid_target"
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string  "connection_not_found"
// @Failure      409  {object}  map[string]string  "integration_disabled"
// @Failure      502  {object}  map[string]string  "integration_degraded"
// @Router       /api/integrations/zernio/connect/pending/{id}/select [post]
func (h *ZernioHandler) SelectPendingConnection(c *fiber.Ctx) error {
	if !h.integ.Enabled() {
		return fiber.NewError(fiber.StatusConflict, "integration_disabled")
	}
	var req selectConnectionRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}
	sess, err := h.loadPendingForTenant(c)
	if err != nil {
		return err
	}

	var opts []zernio.ConnectTarget
	if len(sess.Options) > 0 {
		if err := json.Unmarshal(sess.Options, &opts); err != nil {
			return err
		}
	}
	if !containsTargetID(opts, req.TargetID) {
		return fiber.NewError(fiber.StatusBadRequest, "invalid_target")
	}

	secrets, err := h.openSecrets(sess.SealedSecrets)
	if err != nil {
		return err
	}
	if err := h.integ.Client.SelectConnectTarget(c.Context(), sess.Platform, sess.ProfileID, secrets.TempToken, secrets.ConnectToken, req.TargetID, secrets.UserProfile); err != nil {
		jobs.ZernioConnectSelectFailed.Add(1)
		if apiErr, ok := errors.AsType[*zernio.APIError](err); ok {
			slog.WarnContext(c.Context(), "connect select target failed (picker)",
				logging.AttrComponent, "zernio", "platform", sess.Platform,
				"connectionId", sess.ID, "status", apiErr.Status, "reason", apiErr.Message)
			return fiber.NewError(http.StatusBadGateway, "integration_degraded")
		}
		return err
	}

	_ = h.connectSessions.Delete(c.Context(), sess.ID)
	h.finalizeConnect(c.Context())
	jobs.ZernioConnectSelectSucceeded.Add(1)
	slog.InfoContext(c.Context(), "connect finalized (picker selection)",
		logging.AttrComponent, "zernio", "platform", sess.Platform, "connectionId", sess.ID)
	return c.JSON(fiber.Map{"platform": sess.Platform, "status": "connected"})
}

// loadPendingForTenant loads an awaiting-selection session by :id, asserting it
// is live and belongs to the caller's tenant. Any miss returns 404 without
// revealing whether the id exists for another tenant.
func (h *ZernioHandler) loadPendingForTenant(c *fiber.Ctx) (*models.ZernioConnectSession, error) {
	id := c.Params("id")
	if id == "" {
		return nil, fiber.NewError(fiber.StatusNotFound, "connection_not_found")
	}
	sess, err := h.connectSessions.GetLive(c.Context(), id, time.Now().UTC())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fiber.NewError(fiber.StatusNotFound, "connection_not_found")
		}
		return nil, err
	}
	tenantID, ok := tenantctx.From(c.Context())
	if !ok || sess.TenantID != tenantID || sess.Status != models.ZernioConnectStatusAwaitingSelection {
		return nil, fiber.NewError(fiber.StatusNotFound, "connection_not_found")
	}
	return sess, nil
}

// finalizeConnect tightens the sync cadence and nudges the worker so the freshly
// connected account mirrors into the local table quickly. Mirroring (and the
// account-connected event + usage) stays the sync/reconcile path's job — this
// adds no duplicate write.
func (h *ZernioHandler) finalizeConnect(ctx context.Context) {
	h.integ.BumpFastUntil(time.Now().Add(fastPollWindow))
	if h.worker != nil {
		h.worker.TriggerNow()
	}
	_ = ctx
}

// --- URL builders -----------------------------------------------------------

func (h *ZernioHandler) spaBase() string { return strings.TrimRight(h.appBaseURL, "/") }

// connectCallbackURL is the absolute backend callback Zernio redirects to after
// OAuth, carrying the connect-session id. It must resolve to the API host
// (same-origin with the SPA in the default deploy).
func (h *ZernioHandler) connectCallbackURL(sessionID string) string {
	return h.spaBase() + "/api/integrations/zernio/connect/callback?ogen_cn=" + url.QueryEscape(sessionID)
}

func (h *ZernioHandler) redirectConnectSuccess(c *fiber.Ctx, platform string) error {
	return c.Redirect(h.spaBase()+"/workspace-settings?connected="+url.QueryEscape(platform), fiber.StatusFound)
}

// connectErrorCodeForStatus maps a Zernio select-step HTTP status to the SPA
// connect_error code that best explains the failure, so the user gets advice
// that matches the actual cause instead of a blanket "try again in a moment".
//
//   - 5xx / 408 / 429 → "upstream": a transient reach-the-platform blip; a plain
//     retry can succeed (this is the only case where "try again" is honest).
//   - 401 → "expired": the short-lived tempToken/connect_token no longer
//     authorizes the pick — the link went stale, so the user must reconnect
//     (reuses the existing expired-link copy).
//   - 403 / 400 (and other 4xx) → "permission": the platform did not grant a
//     page/profile we can attach — Page access was not granted on the consent
//     screen, or the account administers nothing postable. A blind retry of the
//     same authorization won't fix it; the user must reconnect and grant access.
//
// The precise Zernio reason is logged at each call site; this only picks the
// user-facing bucket.
func connectErrorCodeForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "expired"
	case http.StatusForbidden, http.StatusBadRequest:
		return "permission"
	}
	return "upstream"
}

func (h *ZernioHandler) redirectConnectError(c *fiber.Ctx, platform, code string) error {
	u := h.spaBase() + "/workspace-settings?connect_error=" + url.QueryEscape(code)
	if platform != "" {
		u += "&platform=" + url.QueryEscape(platform)
	}
	return c.Redirect(u, fiber.StatusFound)
}

func (h *ZernioHandler) redirectPicker(c *fiber.Ctx, connectionID string) error {
	return c.Redirect(h.spaBase()+"/workspace-settings/connect/"+url.PathEscape(connectionID), fiber.StatusFound)
}

// --- token sealing ----------------------------------------------------------

// sealSecrets envelope-encrypts the connect secrets bundle for at-rest storage.
func (h *ZernioHandler) sealSecrets(s connectSecrets) ([]byte, error) {
	plain, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	rec, err := h.cipher.Encrypt(plain)
	if err != nil {
		return nil, err
	}
	return json.Marshal(rec)
}

// openSecrets reverses sealSecrets.
func (h *ZernioHandler) openSecrets(sealed []byte) (connectSecrets, error) {
	var rec envelope.Record
	if err := json.Unmarshal(sealed, &rec); err != nil {
		return connectSecrets{}, err
	}
	plain, err := h.cipher.Decrypt(rec)
	if err != nil {
		return connectSecrets{}, err
	}
	var s connectSecrets
	if err := json.Unmarshal(plain, &s); err != nil {
		return connectSecrets{}, err
	}
	return s, nil
}

// --- small helpers ----------------------------------------------------------

// newConnectSessionID returns a 256-bit unguessable, URL-safe session id. It
// doubles as the OAuth-callback capability and the picker path segment.
func newConnectSessionID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// rawUserProfile passes Zernio's userProfile query param through as raw JSON
// when it is valid JSON, else nil. Fiber has already URL-decoded the value.
func rawUserProfile(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" || !json.Valid([]byte(s)) {
		return nil
	}
	return json.RawMessage(s)
}

func containsTargetID(opts []zernio.ConnectTarget, id string) bool {
	for _, o := range opts {
		if o.ID == id {
			return true
		}
	}
	return false
}
