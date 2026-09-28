package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/usecase/loginsecurity"
)

// Per-IP budgets for the public login-alert endpoints. The token is 256 bits,
// so these only bound load; they are generous for a person on the page.
const (
	loginAlertPreviewPerIP = 30
	loginAlertSecurePerIP  = 10
	loginAlertRateWindow   = time.Hour
)

const loginAlertThrottledMsg = "Too many requests. Please wait a minute and try again."

// LoginAlerts previews and acts on the "This wasn't me" link in a new-device
// alert email. *loginsecurity.Service implements it.
type LoginAlerts interface {
	Preview(ctx context.Context, token string) (*loginsecurity.Preview, error)
	Secure(ctx context.Context, token string) (*loginsecurity.Secured, error)
}

// LoginAlertsHandler serves the two public endpoints behind the alert link.
// The emailed token is the only capability, so both run unauthenticated.
type LoginAlertsHandler struct {
	alerts         LoginAlerts
	previewLimiter *keyedRateLimiter
	secureLimiter  *keyedRateLimiter
}

// NewLoginAlertsHandler builds the handler.
func NewLoginAlertsHandler(alerts LoginAlerts) *LoginAlertsHandler {
	return &LoginAlertsHandler{
		alerts:         alerts,
		previewLimiter: newKeyedRateLimiter(loginAlertPreviewPerIP, loginAlertRateWindow),
		secureLimiter:  newKeyedRateLimiter(loginAlertSecurePerIP, loginAlertRateWindow),
	}
}

func (h *LoginAlertsHandler) Register(app *fiber.App) {
	app.Get("/api/security/login-alerts/:token", h.Preview)
	app.Post("/api/security/login-alerts/:token/secure", h.Secure)
}

// Preview godoc
// @Summary      Preview a new-device login alert
// @Description  Public and read-only: mail scanners follow links with GET, so this never changes anything. Returns the sign-in the alert reported and whether its link is still usable (status pending, used or expired).
// @Tags         security
// @Produce      json
// @Param        token  path      string  true  "Alert token from the email link"
// @Success      200    {object}  loginsecurity.Preview
// @Failure      404    {object}  map[string]string
// @Failure      429    {object}  map[string]string
// @Router       /api/security/login-alerts/{token} [get]
func (h *LoginAlertsHandler) Preview(c *fiber.Ctx) error {
	if ok, retry := h.previewLimiter.allow(c.IP()); !ok {
		return tooManyRequests(c, retry, loginAlertThrottledMsg)
	}
	p, err := h.alerts.Preview(reqCtx(c), c.Params("token"))
	if err != nil {
		return loginAlertError(err)
	}
	return c.JSON(p)
}

// Secure godoc
// @Summary      Secure an account from a new-device login alert
// @Description  Public. Spends the single-use alert token and, atomically, signs the account out of every session, forgets its known devices, voids its other alert links and issues a password-reset link, returned as reset_url. Opens no session. reset_url is empty only when the account has no workspace left.
// @Tags         security
// @Produce      json
// @Param        token  path      string  true  "Alert token from the email link"
// @Success      200    {object}  loginsecurity.Secured
// @Failure      404    {object}  map[string]string
// @Failure      410    {object}  map[string]string  "token_used or token_expired"
// @Failure      429    {object}  map[string]string
// @Router       /api/security/login-alerts/{token}/secure [post]
func (h *LoginAlertsHandler) Secure(c *fiber.Ctx) error {
	if ok, retry := h.secureLimiter.allow(c.IP()); !ok {
		return tooManyRequests(c, retry, loginAlertThrottledMsg)
	}
	res, err := h.alerts.Secure(reqCtx(c), c.Params("token"))
	if err != nil {
		return loginAlertError(err)
	}
	return c.JSON(res)
}

func loginAlertError(err error) error {
	switch {
	case errors.Is(err, loginsecurity.ErrNotFound):
		return fiber.NewError(fiber.StatusNotFound, "not_found")
	case errors.Is(err, loginsecurity.ErrUsed):
		return fiber.NewError(fiber.StatusGone, "token_used")
	case errors.Is(err, loginsecurity.ErrExpired):
		return fiber.NewError(fiber.StatusGone, "token_expired")
	}
	return err
}
