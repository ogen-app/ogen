package handlers

import (
	"context"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/loginsecurity"
)

// LoginSecurity recognises the browsers an account signs in from and alerts
// the owner about new ones. *loginsecurity.Service implements it. Both methods
// return the device-cookie value to set ("" sets nothing) and never fail.
type LoginSecurity interface {
	Observe(ctx context.Context, in loginsecurity.Login) string
	Enroll(ctx context.Context, in loginsecurity.Login) string
}

// deviceTracker is the transport half of known-device tracking shared by the
// three handlers that open sessions: it reads the device cookie and client
// details off the request and writes the cookie back. The zero value is off.
type deviceTracker struct {
	sec        LoginSecurity
	cookieName string
	secure     bool
}

// observe runs new-device detection for a password login.
func (t deviceTracker) observe(c *fiber.Ctx, s *models.Session, email string) {
	if t.sec == nil {
		return
	}
	t.setCookie(c, t.sec.Observe(reqCtx(c), t.login(c, s, email)))
}

// enroll silently marks this browser as known for a brand-new account.
func (t deviceTracker) enroll(c *fiber.Ctx, s *models.Session, email string) {
	if t.sec == nil {
		return
	}
	t.setCookie(c, t.sec.Enroll(reqCtx(c), t.login(c, s, email)))
}

// login copies what it needs off the request. The strings are cloned because
// fiber's values alias fasthttp buffers that are reused once the handler
// returns, and the activity recorder keeps them past that.
func (t deviceTracker) login(c *fiber.Ctx, s *models.Session, email string) loginsecurity.Login {
	return loginsecurity.Login{
		AccountID:   s.AccountID,
		UserID:      s.UserID,
		TenantID:    s.TenantID,
		Email:       email,
		IP:          strings.Clone(c.IP()),
		UserAgent:   string(c.Request().Header.UserAgent()),
		DeviceToken: strings.Clone(c.Cookies(t.cookieName)),
	}
}

// setCookie (re)issues the device cookie with a full lifetime. It is not
// cleared on logout: it identifies the browser, not the session.
func (t deviceTracker) setCookie(c *fiber.Ctx, token string) {
	if token == "" {
		return
	}
	c.Cookie(&fiber.Cookie{
		Name:     t.cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(models.KnownDeviceCookieMaxAge / time.Second),
		Expires:  time.Now().Add(models.KnownDeviceCookieMaxAge),
		HTTPOnly: true,
		Secure:   t.secure,
		SameSite: "Lax",
	})
}
