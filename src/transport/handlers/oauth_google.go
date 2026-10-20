package handlers

import (
	"errors"
	"log/slog"
	"net/url"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// oauthStateCookie carries the raw state of a started Google sign-in. It is
// scoped to the Google routes and lives as long as the state row, so only the
// browser that started a sign-in can finish it (a forged callback link carries
// no cookie).
const (
	oauthStateCookie     = "ogen_oauth_state"
	oauthStateCookiePath = "/api/auth/google"
)

// Callback failures (bad or replayed state, a token that fails verification)
// are charged per client IP like failed password logins. A completed sign-in
// costs nothing.
const (
	oauthFailPerIPBurst = 30
	oauthFailRateWindow = time.Hour
)

// loginPagePath is where a failed sign-in lands with ?error=<code>.
const loginPagePath = "/auth/login"

// GoogleAuthHandler serves sign-in with Google and the provider probe the login
// page reads. A nil service turns Google off: the probe reports it disabled and
// the Google routes are not registered.
type GoogleAuthHandler struct {
	svc *oauthlogin.Service
	// sessions resolves the browser's session for connecting Google from
	// Settings. The Google routes are public, so this is read directly rather
	// than through the auth middleware.
	sessions     repository.SessionRepository
	cookieName   string
	secureCookie bool
	failLimiter  *keyedRateLimiter
	// signupLimiter is the per-IP signup budget; ShareSignupThrottle makes it
	// the same bucket POST /api/tenants spends, so the two signup paths can't
	// be alternated to double the budget.
	signupLimiter *keyedRateLimiter
	activity      *activity.Recorder
	devices       deviceTracker
}

// NewGoogleAuthHandler returns the handler; svc may be nil.
func NewGoogleAuthHandler(svc *oauthlogin.Service, sessions repository.SessionRepository, cookieName string, secureCookie bool, rec *activity.Recorder) *GoogleAuthHandler {
	return &GoogleAuthHandler{
		svc:           svc,
		sessions:      sessions,
		cookieName:    cookieName,
		secureCookie:  secureCookie,
		failLimiter:   newKeyedRateLimiter(oauthFailPerIPBurst, oauthFailRateWindow),
		signupLimiter: newKeyedRateLimiter(signupPerIPBurst, signupRateWindow),
		activity:      rec,
	}
}

// SetLoginSecurity runs new-device detection on Google sign-ins.
func (h *GoogleAuthHandler) SetLoginSecurity(sec LoginSecurity, cookieName string) {
	h.devices = deviceTracker{sec: sec, cookieName: cookieName, secure: h.secureCookie}
}

func (h *GoogleAuthHandler) Register(app *fiber.App) {
	app.Get("/api/auth/providers", h.Providers)
	if h.svc == nil {
		return
	}
	g := app.Group(oauthStateCookiePath)
	g.Get("/start", h.Start)
	g.Get("/callback", h.Callback)
	g.Get("/pending/:token", h.Pending)
	g.Post("/complete", h.Complete)
}

type providerStatus struct {
	Enabled bool `json:"enabled"`
}

type providersResponse struct {
	Google providerStatus `json:"google"`
}

// Providers godoc
// @Summary      Sign-in providers
// @Description  Reports which external sign-in providers are configured, so the login and register pages show only working buttons.
// @Tags         auth
// @Produce      json
// @Success      200  {object}  providersResponse
// @Router       /api/auth/providers [get]
func (h *GoogleAuthHandler) Providers(c *fiber.Ctx) error {
	return c.JSON(providersResponse{Google: providerStatus{Enabled: h.svc != nil}})
}

// Start godoc
// @Summary      Start Google sign-in
// @Description  Sends the browser to Google. Navigate to it (it is not an XHR endpoint). The same flow signs in an existing account or starts a sign-up; the callback decides.
// @Tags         auth
// @Param        intent     query  string  false  "signin (default), or link to connect Google to the signed-in account"
// @Param        return_to  query  string  false  "Same-site path to land on after signing in"
// @Success      302
// @Failure      400  {object}  map[string]string
// @Router       /api/auth/google/start [get]
func (h *GoogleAuthHandler) Start(c *fiber.Ctx) error {
	in := oauthlogin.StartInput{
		Intent:   c.Query("intent"),
		ReturnTo: c.Query("return_to"),
	}
	if in.Intent == models.OAuthIntentLink {
		if s := h.currentSession(c); s != nil {
			in.AccountID = s.AccountID
		}
	}
	started, err := h.svc.Start(reqCtx(c), in)
	switch {
	case errors.Is(err, oauthlogin.ErrInvalidIntent):
		return fiber.NewError(fiber.StatusBadRequest, "unsupported intent")
	case errors.Is(err, oauthlogin.ErrNotSignedIn):
		return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	if err != nil {
		return err
	}
	c.Cookie(&fiber.Cookie{
		Name:     oauthStateCookie,
		Value:    started.State,
		Path:     oauthStateCookiePath,
		MaxAge:   int(models.OAuthStateTTL.Seconds()),
		HTTPOnly: true,
		Secure:   h.secureCookie,
		SameSite: "Lax",
	})
	return c.Redirect(started.AuthURL, fiber.StatusFound)
}

// Callback godoc
// @Summary      Google sign-in callback
// @Description  Google redirects here. Always answers 302: into the app with a session cookie on success, or to the login page with ?error=<code>.
// @Tags         auth
// @Param        code   query  string  false  "Authorization code"
// @Param        state  query  string  false  "State issued by start"
// @Param        error  query  string  false  "Set by Google when the user cancelled"
// @Success      302
// @Router       /api/auth/google/callback [get]
func (h *GoogleAuthHandler) Callback(c *fiber.Ctx) error {
	ip := c.IP()
	cookieState := c.Cookies(oauthStateCookie)
	h.clearStateCookie(c)
	if h.failLimiter.retryAfter(ip) > 0 {
		return h.redirectError(c, &oauthlogin.Error{Code: oauthlogin.CodeThrottled})
	}

	in := oauthlogin.CallbackInput{
		State:         c.Query("state"),
		CookieState:   cookieState,
		Code:          c.Query("code"),
		ProviderError: c.Query("error"),
	}
	session := h.currentSession(c)
	if session != nil {
		in.SessionAccountID = session.AccountID
	}
	res, err := h.svc.Callback(reqCtx(c), in)
	if err != nil {
		e, ok := errors.AsType[*oauthlogin.Error](err)
		if !ok {
			slog.ErrorContext(reqCtx(c), "google sign-in failed",
				logging.AttrComponent, "handlers.oauth_google", logging.AttrError, err)
			e = &oauthlogin.Error{Code: oauthlogin.CodeUnavailable}
		}
		if e.Code == oauthlogin.CodeStateInvalid {
			h.failLimiter.penalize(ip)
		}
		return h.redirectError(c, e)
	}
	switch res.Kind {
	case oauthlogin.ResultPendingSignup:
		return c.Redirect(withQuery(registerGooglePath, "token", res.PendingToken), fiber.StatusFound)
	case oauthlogin.ResultConnected:
		if res.Linked {
			h.activity.Record(withActor(reqCtx(c), session.TenantID, session.UserID), activity.CategoryAuthentication, "identity_linked",
				activity.WithEntity("user", session.UserID), activity.WithSource(activity.SourceAPI),
				activity.WithPayload(map[string]any{"provider": h.svc.ProviderName(), "via": res.Intent, "password_cleared": false}))
		}
		return c.Redirect(withQuery(res.ReturnTo, "linked", h.svc.ProviderName()), fiber.StatusFound)
	}
	return h.signedIn(c, res)
}

// currentSession returns the browser's live session, or nil when it has none.
func (h *GoogleAuthHandler) currentSession(c *fiber.Ctx) *models.Session {
	token := c.Cookies(h.cookieName)
	if token == "" || h.sessions == nil {
		return nil
	}
	s, err := h.sessions.GetByID(reqCtx(c), token)
	if err != nil || !time.Now().UTC().Before(s.ExpiresAt) {
		return nil
	}
	return s
}

// registerGooglePath is the UI step where a newcomer names their workspace.
const registerGooglePath = "/auth/register/google"

// signedIn sets the session cookie, records the events, runs new-device
// detection and lands the browser on its return path.
func (h *GoogleAuthHandler) signedIn(c *fiber.Ctx, res *oauthlogin.Result) error {
	setSessionCookie(c, h.cookieName, res.Session, h.secureCookie)
	actor := withActor(reqCtx(c), res.Session.TenantID, res.User.ID)
	if res.Linked {
		h.activity.Record(actor, activity.CategoryAuthentication, "identity_linked",
			activity.WithEntity("user", res.User.ID), activity.WithSource(activity.SourceAPI),
			activity.WithPayload(map[string]any{"provider": h.svc.ProviderName(), "via": res.Intent, "password_cleared": res.PasswordCleared}))
	}
	h.activity.Record(actor, activity.CategoryAuthentication, "login",
		activity.WithEntity("user", res.User.ID), activity.WithSource(activity.SourceAPI),
		activity.WithPayload(map[string]any{"method": h.svc.ProviderName()}))
	h.devices.observe(c, res.Session, res.Account.Email)
	return c.Redirect(res.ReturnTo, fiber.StatusFound)
}

// Pending godoc
// @Summary      Preview a Google sign-up
// @Description  Returns the verified Google address behind a pending sign-up, for the "name your workspace" step. Unknown, used and expired tokens all answer 404.
// @Tags         auth
// @Produce      json
// @Param        token  path      string  true  "Pending sign-up token from the callback redirect"
// @Success      200    {object}  oauthlogin.PendingPreview
// @Failure      404    {object}  map[string]string
// @Router       /api/auth/google/pending/{token} [get]
func (h *GoogleAuthHandler) Pending(c *fiber.Ctx) error {
	p, err := h.svc.PendingSignup(reqCtx(c), c.Params("token"))
	if errors.Is(err, oauthlogin.ErrPendingNotFound) {
		return fiber.NewError(fiber.StatusNotFound, pendingNotFoundMsg)
	}
	if err != nil {
		return err
	}
	return c.JSON(p)
}

const pendingNotFoundMsg = "this sign-up link has expired; continue with Google again"

type completeGoogleSignupRequest struct {
	Token      string `json:"token"       validate:"required"`
	TenantName string `json:"tenant_name" validate:"required,max=200"`
}

type completeGoogleSignupResponse struct {
	Tenant   *models.Tenant  `json:"tenant"`
	User     *models.User    `json:"user"`
	Session  *models.Session `json:"session"`
	ReturnTo string          `json:"return_to"`
}

// Complete godoc
// @Summary      Complete a Google sign-up
// @Description  Creates the workspace, a passwordless account linked to the Google identity, and a session (cookie set). Shares the per-IP throttle of POST /api/tenants. 409 bodies carry a code: email_in_use or google_already_linked.
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body      completeGoogleSignupRequest  true  "Pending token and workspace name"
// @Success      201   {object}  completeGoogleSignupResponse
// @Failure      400   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      409   {object}  map[string]string
// @Failure      429   {object}  map[string]string
// @Router       /api/auth/google/complete [post]
func (h *GoogleAuthHandler) Complete(c *fiber.Ctx) error {
	var req completeGoogleSignupRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}
	if ok, retry := h.signupLimiter.allow(c.IP()); !ok {
		return tooManyRequests(c, retry, signupThrottledMsg)
	}
	res, err := h.svc.CompleteSignup(reqCtx(c), req.Token, req.TenantName)
	switch {
	case errors.Is(err, oauthlogin.ErrPendingNotFound):
		return fiber.NewError(fiber.StatusNotFound, pendingNotFoundMsg)
	case errors.Is(err, signup.ErrEmailInUse):
		return rejectCode(c, fiber.StatusConflict, "email_in_use", "an account with this email already exists; continue with Google to sign in")
	case errors.Is(err, signup.ErrIdentityLinked):
		return rejectCode(c, fiber.StatusConflict, "google_already_linked", "this Google account already signs in to Ogen; continue with Google to sign in")
	case err != nil:
		return err
	}

	setSessionCookie(c, h.cookieName, res.Session, h.secureCookie)
	h.devices.enroll(c, res.Session, res.User.Email)
	h.activity.Record(withActor(reqCtx(c), res.Tenant.ID, res.User.ID), activity.CategoryAuthentication, "signup",
		activity.WithEntity("tenant", res.Tenant.ID), activity.WithSource(activity.SourceAPI),
		activity.WithPayload(map[string]any{"method": h.svc.ProviderName()}))
	return c.Status(fiber.StatusCreated).JSON(completeGoogleSignupResponse{
		Tenant: res.Tenant, User: res.User, Session: res.Session, ReturnTo: res.ReturnTo,
	})
}

func (h *GoogleAuthHandler) clearStateCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name:     oauthStateCookie,
		Value:    "",
		Path:     oauthStateCookiePath,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HTTPOnly: true,
		Secure:   h.secureCookie,
		SameSite: "Lax",
	})
}

// redirectError lands a failed sign-in on the login page with its code. A
// Settings flow (connecting Google while signed in) goes back to where it
// started instead.
func (h *GoogleAuthHandler) redirectError(c *fiber.Ctx, e *oauthlogin.Error) error {
	target := loginPagePath
	if e.Intent == models.OAuthIntentLink && e.ReturnTo != "" {
		target = e.ReturnTo
	}
	return c.Redirect(withQuery(target, "error", e.Code), fiber.StatusFound)
}

// withQuery sets one query parameter on a same-site path.
func withQuery(path, key, value string) string {
	u, err := url.Parse(path)
	if err != nil {
		return loginPagePath + "?" + url.Values{key: {value}}.Encode()
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}
