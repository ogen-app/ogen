package handlers

import (
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/crypto/envelope"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// fastPollWindow is the fast-cadence window the worker honours after a
// connect link is issued. Per the ticket: 10 minutes.
const fastPollWindow = 10 * time.Minute

// connectSessionTTL bounds a headless connect session's lifetime (CON-217) and
// is the expiry advertised as connectLinkResponse.ExpiresAt. It matches Zernio's
// 15-minute connect_token expiry: the whole connect flow (open link → authorize
// → any in-Ogen pick) must complete within it, since the session row is not
// extended on the awaiting_selection transition. A callback or pick arriving
// later finds the session gone and fails closed.
const connectSessionTTL = 15 * time.Minute

// ZernioHandler exposes the integration endpoints under
// /api/integrations/zernio.
type ZernioHandler struct {
	integ        *zernio.Integration
	bootstrapper *zernio.Bootstrapper
	settings     zernio.SettingsStore
	platforms    repository.PlatformRepository
	accounts     repository.SocialAccountRepository
	posts        repository.PostRepository
	worker       *zernio.Worker
	rateLimiter  *zernio.RateLimiter
	auth         fiber.Handler

	// CON-217: headless connect flow. connectSessions holds the short-lived
	// per-connect state; cipher seals the Zernio connect_token/tempToken at
	// rest; appBaseURL builds the absolute backend callback + SPA landing URLs.
	connectSessions repository.ZernioConnectSessionRepository
	cipher          *envelope.Cipher
	appBaseURL      string
}

func NewZernioHandler(
	integ *zernio.Integration,
	bootstrapper *zernio.Bootstrapper,
	settings zernio.SettingsStore,
	platforms repository.PlatformRepository,
	accounts repository.SocialAccountRepository,
	posts repository.PostRepository,
	worker *zernio.Worker,
	rateLimiter *zernio.RateLimiter,
	auth fiber.Handler,
	connectSessions repository.ZernioConnectSessionRepository,
	cipher *envelope.Cipher,
	appBaseURL string,
) *ZernioHandler {
	return &ZernioHandler{
		integ:           integ,
		bootstrapper:    bootstrapper,
		settings:        settings,
		platforms:       platforms,
		accounts:        accounts,
		posts:           posts,
		worker:          worker,
		rateLimiter:     rateLimiter,
		auth:            auth,
		connectSessions: connectSessions,
		cipher:          cipher,
		appBaseURL:      appBaseURL,
	}
}

func (h *ZernioHandler) Register(app *fiber.App) {
	// Health is intentionally unauthenticated, matching /api/health —
	// monitoring agents need to scrape it without holding a session.
	app.Get("/api/integrations/zernio/health", h.Health)

	// CON-217: the headless OAuth callback is a browser redirect target from
	// Zernio, so it sits OUTSIDE the cookie-auth group — the session cookie may
	// not survive the cross-site redirect. It authenticates via the unguessable
	// connect-session id (ogen_cn) instead.
	app.Get("/api/integrations/zernio/connect/callback", h.ConnectCallback)

	g := app.Group("/api/integrations/zernio", h.auth)
	g.Get("/platforms", h.ListPlatforms)
	g.Post("/connect-links", h.CreateConnectLink)
	// CON-217: in-Ogen picker for multi-target connects (LinkedIn orgs, FB pages).
	g.Get("/connect/pending/:id", h.GetPendingConnection)
	g.Post("/connect/pending/:id/select", h.SelectPendingConnection)
	g.Get("/accounts", h.ListAccounts)
	g.Delete("/accounts/:id", h.DisconnectAccount)
	g.Post("/sync", h.TriggerSync)
	g.Post("/profile/repair", h.RepairProfile)
}

type healthResponse struct {
	Enabled        bool   `json:"enabled"`
	State          string `json:"state"`
	ProfileID      string `json:"profileId,omitempty"`
	LastSyncAt     string `json:"lastSyncAt,omitempty"`
	LastSyncStatus string `json:"lastSyncStatus,omitempty"`
	// AccountCount is per-tenant, so it is a pointer that stays nil (and is
	// omitted) on the unauthenticated/tenantless path — a bare "accountCount":0
	// there would falsely read as "this tenant has 0 accounts". It is set only
	// inside the tenant-scoped branch below, after ListActive.
	AccountCount *int `json:"accountCount,omitempty"`
}

// Health godoc
// @Summary      Zernio integration health
// @Description  Public endpoint suitable for inclusion in monitoring
// @Description  dashboards. Reports the app-wide enabled flag and state
// @Description  (disabled / degraded / ok). Per-tenant profile and sync
// @Description  detail is included only when the caller carries a tenant.
// @Tags         zernio
// @Produce      json
// @Success      200  {object}  healthResponse
// @Router       /api/integrations/zernio/health [get]
func (h *ZernioHandler) Health(c *fiber.Ctx) error {
	resp := healthResponse{
		Enabled: h.integ.Enabled(),
		State:   string(h.integ.State()),
	}
	// enabled/state are app-wide — they reflect the shared zernio_api_key, not
	// any tenant. The profile, last-sync, and account count are per-tenant
	// (CON-100), so only include them when the caller carries a tenant. This
	// endpoint is unauthenticated, so it must not run a tenant-scoped query
	// without one (it would fail closed with ErrNoTenant).
	if _, ok := tenantctx.From(reqCtx(c)); ok {
		profileID, _, err := h.settings.Get(reqCtx(c), zernio.SettingProfileID)
		if err != nil {
			return err
		}
		resp.ProfileID = profileID
		if profileID != "" {
			rows, err := h.accounts.ListActive(reqCtx(c), profileID)
			if err != nil {
				return err
			}
			count := len(rows)
			resp.AccountCount = &count
		}
		// Surface real store I/O failures instead of masking them as empty
		// metadata (consistent with the profileID read above). A missing row
		// (found=false, err=nil) legitimately yields "" and is omitted.
		lastAt, _, err := h.settings.Get(reqCtx(c), zernio.SettingLastSyncAt)
		if err != nil {
			return err
		}
		lastStatus, _, err := h.settings.Get(reqCtx(c), zernio.SettingLastSyncStatus)
		if err != nil {
			return err
		}
		resp.LastSyncAt = lastAt
		resp.LastSyncStatus = lastStatus
	}
	return c.JSON(resp)
}

// platformInfo is the GET /platforms entry shape.
type platformInfo struct {
	ID                 string   `json:"id"`
	Label              string   `json:"label"`
	SupportedPostTypes []string `json:"supportedPostTypes"`
}

type platformsResponse struct {
	Platforms []platformInfo `json:"platforms"`
}

// ListPlatforms godoc
// @Summary      List Zernio-supported platforms (Phase 1 allowlist)
// @Description  Deprecated: prefer GET /api/platforms, which surfaces the
// @Description  same publisher information (and connection state) inline
// @Description  per platform. This endpoint stays for clients that
// @Description  haven't migrated yet and will be removed in a later
// @Description  cycle.
// @Description
// @Description  Returns the Phase 1 allowlist with each platform's
// @Description  supportedPostTypes derived from the local platforms
// @Description  table.
// @Tags         zernio
// @Deprecated
// @Produce      json
// @Security     CookieAuth
// @Success      200  {object}  platformsResponse
// @Failure      401  {object}  map[string]string
// @Router       /api/integrations/zernio/platforms [get]
func (h *ZernioHandler) ListPlatforms(c *fiber.Ctx) error {
	allowlist := zernio.SupportedPlatforms()
	out := make([]platformInfo, 0, len(allowlist))
	for _, p := range allowlist {
		platform, err := h.platforms.GetByID(reqCtx(c), p.OgenID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// A missing local platforms row is treated as "no post types known"
		// rather than a failure — the allowlist is the source of truth for
		// what Zernio supports; the post-types map is best-effort metadata.
		var postTypes []string
		if platform != nil {
			postTypes = make([]string, 0, len(platform.PostTypes))
			for slug := range platform.PostTypes {
				postTypes = append(postTypes, slug)
			}
		}
		out = append(out, platformInfo{
			ID:                 p.ZernioID,
			Label:              p.Label,
			SupportedPostTypes: postTypes,
		})
	}
	return c.JSON(platformsResponse{Platforms: out})
}

type connectLinkRequest struct {
	Platform string `json:"platform" validate:"required"`
}

type connectLinkResponse struct {
	Platform   string    `json:"platform"`
	ConnectURL string    `json:"connectUrl"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// CreateConnectLink godoc
// @Summary      Create a Zernio connect link
// @Description  Returns a one-shot connect URL that the user opens in
// @Description  a browser to authorize a social account on the chosen
// @Description  platform. The URL contains a short-lived token.
// @Tags         zernio
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      connectLinkRequest  true  "Platform to connect"
// @Success      200   {object}  connectLinkResponse
// @Failure      400   {object}  map[string]string  "invalid_platform"
// @Failure      401   {object}  map[string]string
// @Failure      409   {object}  map[string]string  "integration_disabled"
// @Failure      429   {object}  map[string]string  "rate_limited"
// @Failure      503   {object}  map[string]string  "integration_degraded"
// @Router       /api/integrations/zernio/connect-links [post]
func (h *ZernioHandler) CreateConnectLink(c *fiber.Ctx) error {
	if !h.integ.Enabled() {
		return fiber.NewError(fiber.StatusConflict, "integration_disabled")
	}

	var req connectLinkRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}
	if zernio.LookupSupportedPlatform(req.Platform) == nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid_platform")
	}

	if allow, retryAfter := h.rateLimiter.Allow(); !allow {
		seconds := int(math.Ceil(retryAfter.Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		c.Set("Retry-After", strconv.Itoa(seconds))
		return fiber.NewError(fiber.StatusTooManyRequests, "rate_limited")
	}

	profileID, ok, err := h.settings.Get(reqCtx(c), zernio.SettingProfileID)
	if err != nil {
		return err
	}
	if !ok || profileID == "" {
		// CON-100: lazily bootstrap THIS tenant's Zernio profile on its first
		// connect (the request context carries the tenant), then re-read it.
		if err := h.bootstrapper.Run(reqCtx(c)); err != nil {
			return fiber.NewError(fiber.StatusServiceUnavailable, "integration_degraded")
		}
		profileID, ok, err = h.settings.Get(reqCtx(c), zernio.SettingProfileID)
		if err != nil {
			return err
		}
		if !ok || profileID == "" {
			return fiber.NewError(fiber.StatusServiceUnavailable, "integration_degraded")
		}
	}

	// Health gate runs *after* the lazy bootstrap above: a missing-profile
	// bootstrap promotes a transient StateDegraded back to StateOK on success
	// (CON-100), so a first connect can self-heal instead of being short-
	// circuited to 503 before it ever gets the chance.
	if h.integ.State() != zernio.StateOK {
		return fiber.NewError(fiber.StatusServiceUnavailable, "integration_degraded")
	}

	// CON-217: mint a short-lived connect session so the headless OAuth callback
	// can resolve this tenant + profile without relying on the browser session
	// (which may not survive the cross-site redirect through Zernio), and hold
	// the pending selection when a platform has 2+ targets. The session id rides
	// the callback URL as ogen_cn.
	tenantID, ok := tenantctx.From(reqCtx(c))
	if !ok {
		return fiber.NewError(fiber.StatusUnauthorized, "no_tenant")
	}
	sessionID, err := newConnectSessionID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := h.connectSessions.Create(reqCtx(c), &models.ZernioConnectSession{
		ID:        sessionID,
		TenantID:  tenantID,
		ProfileID: profileID,
		Platform:  req.Platform,
		Status:    models.ZernioConnectStatusPendingAuth,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(connectSessionTTL),
	}); err != nil {
		return err
	}

	connectURL, err := h.integ.Client.CreateConnectLink(reqCtx(c), profileID, req.Platform, h.connectCallbackURL(sessionID))
	if err != nil {
		if apiErr, ok := errors.AsType[*zernio.APIError](err); ok {
			return fiber.NewError(http.StatusBadGateway, apiErr.Error())
		}
		return err
	}

	// Adaptive polling window: tighten the worker cadence for the next
	// fastPollWindow so the user sees their connected account fast.
	h.integ.BumpFastUntil(time.Now().Add(fastPollWindow))

	// CON-102: mark this tenant as having initiated a Zernio connection so the
	// background sync worker starts sweeping it. With eager provisioning every
	// tenant has a profile from signup, so the worker keys its sweep on this
	// marker — not mere profile presence — to avoid polling tenants that never
	// connected. This marker is the *only* sweep selector for the tenant, so the
	// write must be durable: surface store failures as a retryable error rather
	// than silently stranding the account the user is about to authorize (they
	// already received a link, so they would not retry on their own). Written once.
	marker, ok, err := h.settings.Get(reqCtx(c), zernio.SettingConnectInitiatedAt)
	if err != nil {
		return err
	}
	if !ok || marker == "" {
		if err := h.settings.Set(reqCtx(c), zernio.SettingConnectInitiatedAt, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}

	// Log redacted URL only — the query string contains a short-lived
	// token that must not appear in log lines.
	slog.InfoContext(reqCtx(c), "connect link issued", logging.AttrComponent, "zernio", "platform", req.Platform, "profile", profileID, "url", redactConnectURL(connectURL))

	return c.JSON(connectLinkResponse{
		Platform:   req.Platform,
		ConnectURL: connectURL,
		ExpiresAt:  now.Add(connectSessionTTL),
	})
}

type accountsResponse struct {
	Accounts       []accountInfo `json:"accounts"`
	LastSyncAt     string        `json:"lastSyncAt,omitempty"`
	LastSyncStatus string        `json:"lastSyncStatus,omitempty"`
}

type accountInfo struct {
	ID           string    `json:"id"`
	Platform     string    `json:"platform"`
	Username     string    `json:"username"`
	DisplayName  string    `json:"displayName"`
	AvatarURL    string    `json:"avatarUrl"`
	IsActive     bool      `json:"isActive"`
	ConnectedAt  time.Time `json:"connectedAt"`
	LastSyncedAt time.Time `json:"lastSyncedAt"`
}

// ListAccounts godoc
// @Summary      List Zernio social accounts (local mirror)
// @Description  Returns the local view of accounts attached via the
// @Description  Zernio profile, plus the last sync timestamp + status.
// @Description  Reads from the database — does not call Zernio.
// @Tags         zernio
// @Produce      json
// @Security     CookieAuth
// @Success      200  {object}  accountsResponse
// @Failure      401  {object}  map[string]string
// @Failure      409  {object}  map[string]string  "integration_disabled"
// @Router       /api/integrations/zernio/accounts [get]
func (h *ZernioHandler) ListAccounts(c *fiber.Ctx) error {
	if !h.integ.Enabled() {
		return fiber.NewError(fiber.StatusConflict, "integration_disabled")
	}
	profileID, _, err := h.settings.Get(reqCtx(c), zernio.SettingProfileID)
	if err != nil {
		return err
	}
	var rows []accountInfo
	if profileID != "" {
		fetched, err := h.accounts.ListActive(reqCtx(c), profileID)
		if err != nil {
			return err
		}
		rows = make([]accountInfo, 0, len(fetched))
		for _, a := range fetched {
			rows = append(rows, accountInfo{
				ID:           a.ID,
				Platform:     a.Platform,
				Username:     a.Username,
				DisplayName:  a.DisplayName,
				AvatarURL:    a.AvatarURL,
				IsActive:     a.IsActive,
				ConnectedAt:  a.ConnectedAt,
				LastSyncedAt: a.LastSyncedAt,
			})
		}
	}
	lastAt, _, _ := h.settings.Get(reqCtx(c), zernio.SettingLastSyncAt)
	lastStatus, _, _ := h.settings.Get(reqCtx(c), zernio.SettingLastSyncStatus)
	return c.JSON(accountsResponse{
		Accounts:       rows,
		LastSyncAt:     lastAt,
		LastSyncStatus: lastStatus,
	})
}

// DisconnectAccount godoc
// @Summary      Disconnect a Zernio social account from the tenant
// @Description  Removes a connected social account: deletes it upstream on
// @Description  Zernio (DELETE /accounts/{id}), then soft-deletes the local
// @Description  mirror row so a later sync doesn't revive it. Blocked with 409
// @Description  when the account still has scheduled posts, unless force=true.
// @Description  Idempotent: an unknown or already-disconnected id returns 404.
// @Tags         zernio
// @Produce      json
// @Security     CookieAuth
// @Param        id     path   string  true   "Social account id"
// @Param        force  query  boolean false  "Disconnect even if scheduled posts reference the account"
// @Success      204  "Disconnected"
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string  "account_not_found"
// @Failure      409  {object}  map[string]string  "integration_disabled | account_has_scheduled_posts"
// @Failure      502  {object}  map[string]string  "integration_degraded"
// @Router       /api/integrations/zernio/accounts/{id} [delete]
func (h *ZernioHandler) DisconnectAccount(c *fiber.Ctx) error {
	if !h.integ.Enabled() {
		return fiber.NewError(fiber.StatusConflict, "integration_disabled")
	}

	id := c.Params("id")
	if id == "" {
		return fiber.NewError(fiber.StatusNotFound, "account_not_found")
	}
	force := c.QueryBool("force", false)

	profileID, _, err := h.settings.Get(reqCtx(c), zernio.SettingProfileID)
	if err != nil {
		return err
	}
	if profileID == "" {
		// No profile → this tenant has never connected anything to disconnect.
		return fiber.NewError(fiber.StatusNotFound, "account_not_found")
	}

	account, err := h.accounts.GetActive(reqCtx(c), profileID, id)
	if err != nil {
		return notFound(err, "account_not_found")
	}

	// Refuse to strand scheduled posts unless the caller forces it.
	if !force {
		pending, err := h.posts.CountPendingByAccount(reqCtx(c), id)
		if err != nil {
			return err
		}
		if pending > 0 {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"error":          "account_has_scheduled_posts",
				"scheduledPosts": pending,
			})
		}
	}

	// Remove upstream first so the local soft-delete sticks: the reconciler
	// revives any soft-deleted row Zernio still returns (reconcile.go), so a
	// local-only delete would bounce back on the next sync. A 404 upstream means
	// the account is already gone there — treat it as an idempotent no-op and
	// proceed to heal local state. Any other upstream failure stops here (no
	// local write) so Ogen and Zernio can't diverge.
	if err := h.integ.Client.DeleteAccount(reqCtx(c), id); err != nil {
		if !zernio.IsStatus(err, http.StatusNotFound) {
			jobs.ZernioAccountDisconnectFailed.Add(1)
			slog.ErrorContext(reqCtx(c), "zernio account delete failed",
				logging.AttrComponent, "zernio",
				"account_id", id,
				"platform", account.Platform,
				logging.AttrError, err)
			return fiber.NewError(http.StatusBadGateway, "integration_degraded")
		}
		slog.InfoContext(reqCtx(c), "zernio account already absent upstream; healing local state",
			logging.AttrComponent, "zernio",
			"account_id", id)
	}

	updated, err := h.accounts.SoftDelete(reqCtx(c), id, time.Now().UTC())
	if err != nil {
		jobs.ZernioAccountDisconnectFailed.Add(1)
		return err
	}
	if !updated {
		// A concurrent disconnect (another request, or the reconciler seeing the
		// account gone upstream) already soft-deleted this row between GetActive
		// and here. The account is gone, so the caller's intent holds — return
		// success, but skip the event / usage / success telemetry so the
		// concurrent winner isn't double-counted.
		slog.InfoContext(reqCtx(c), "account already disconnected concurrently; skipping duplicate side effects",
			logging.AttrComponent, "zernio",
			"account_id", id)
		return c.SendStatus(fiber.StatusNoContent)
	}

	// Best-effort event + usage; skipped when no worker is wired (e.g. in tests).
	if h.worker != nil {
		h.worker.PublishAccountDisconnected(reqCtx(c), *account)
		h.worker.RecordAccountDisconnect(reqCtx(c), *account)
	}

	jobs.ZernioAccountDisconnectSucceeded.Add(1)
	slog.InfoContext(reqCtx(c), "account disconnected (user-initiated)",
		logging.AttrComponent, "zernio",
		"profile_id", profileID,
		"platform", account.Platform,
		"account_id", id,
		"forced", force)
	return c.SendStatus(fiber.StatusNoContent)
}

// TriggerSync godoc
// @Summary      Trigger a Zernio sync tick
// @Description  Asks the background worker to run a sync at its
// @Description  earliest opportunity. Returns 202 with the previous
// @Description  last_sync_at value so callers can poll for progress.
// @Tags         zernio
// @Produce      json
// @Security     CookieAuth
// @Success      202  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      409  {object}  map[string]string  "integration_disabled"
// @Router       /api/integrations/zernio/sync [post]
func (h *ZernioHandler) TriggerSync(c *fiber.Ctx) error {
	if !h.integ.Enabled() {
		return fiber.NewError(fiber.StatusConflict, "integration_disabled")
	}
	prevAt, _, _ := h.settings.Get(reqCtx(c), zernio.SettingLastSyncAt)
	if h.worker != nil {
		h.worker.TriggerNow()
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"previousLastSyncAt": prevAt,
	})
}

// RepairProfile godoc
// @Summary      Repair Zernio profile
// @Description  Triggers a fresh Zernio profile bootstrap for operational
// @Description  recovery. Idempotent: a successful repair leaves the
// @Description  integration in the "ok" state.
// @Tags         zernio
// @Produce      json
// @Security     CookieAuth
// @Success      202  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      409  {object}  map[string]string  "integration_disabled"
// @Failure      500  {object}  map[string]string
// @Router       /api/integrations/zernio/profile/repair [post]
func (h *ZernioHandler) RepairProfile(c *fiber.Ctx) error {
	if !h.integ.Enabled() {
		return fiber.NewError(fiber.StatusConflict, "integration_disabled")
	}
	if err := h.bootstrapper.Run(reqCtx(c)); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, err.Error())
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"state": string(h.integ.State()),
	})
}

// redactConnectURL strips the query string from a connect URL so log
// lines record host + path without the short-lived token.
func redactConnectURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "<invalid url>"
	}
	return u.Scheme + "://" + u.Host + u.Path
}
