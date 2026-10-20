package handlers

import (
	"errors"
	"log/slog"
	"net/url"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin"
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
	svc          *oauthlogin.Service
	cookieName   string
	secureCookie bool
	failLimiter  *keyedRateLimiter
	activity     *activity.Recorder
	devices      deviceTracker
}

// NewGoogleAuthHandler returns the handler; svc may be nil.
func NewGoogleAuthHandler(svc *oauthlogin.Service, cookieName string, secureCookie bool, rec *activity.Recorder) *GoogleAuthHandler {
	return &GoogleAuthHandler{
		svc:          svc,
		cookieName:   cookieName,
		secureCookie: secureCookie,
		failLimiter:  newKeyedRateLimiter(oauthFailPerIPBurst, oauthFailRateWindow),
		activity:     rec,
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
// @Param        intent     query  string  false  "signin (default)"
// @Param        return_to  query  string  false  "Same-site path to land on after signing in"
// @Success      302
// @Failure      400  {object}  map[string]string
// @Router       /api/auth/google/start [get]
func (h *GoogleAuthHandler) Start(c *fiber.Ctx) error {
	started, err := h.svc.Start(reqCtx(c), oauthlogin.StartInput{
		Intent:   c.Query("intent"),
		ReturnTo: c.Query("return_to"),
	})
	if errors.Is(err, oauthlogin.ErrInvalidIntent) {
		return fiber.NewError(fiber.StatusBadRequest, "unsupported intent")
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

	res, err := h.svc.Callback(reqCtx(c), oauthlogin.CallbackInput{
		State:         c.Query("state"),
		CookieState:   cookieState,
		Code:          c.Query("code"),
		ProviderError: c.Query("error"),
	})
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
	return h.signedIn(c, res, "login")
}

// signedIn sets the session cookie, records the event, runs new-device
// detection and lands the browser on its return path.
func (h *GoogleAuthHandler) signedIn(c *fiber.Ctx, res *oauthlogin.Result, action string) error {
	setSessionCookie(c, h.cookieName, res.Session, h.secureCookie)
	h.activity.Record(withActor(reqCtx(c), res.Session.TenantID, res.User.ID), activity.CategoryAuthentication, action,
		activity.WithEntity("user", res.User.ID), activity.WithSource(activity.SourceAPI),
		activity.WithPayload(map[string]any{"method": h.svc.ProviderName()}))
	h.devices.observe(c, res.Session, res.Account.Email)
	return c.Redirect(res.ReturnTo, fiber.StatusFound)
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
