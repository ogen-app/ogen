package handlers

import (
	"context"
	"errors"
	"html"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// SecretResolver resolves a named secret (e.g. email_link_secret) per call so a
// rotation takes effect with no restart. Satisfied by an adapter over
// secrets.Store in server wiring.
type SecretResolver func(ctx context.Context) (string, error)

// EmailHandler serves the public marketing-unsubscribe surface (CON-154/CON-155).
// The link (GET) verifies the token and renders a confirmation page but NEVER
// mutates — so a mail scanner / link prefetcher issuing a GET can't unsubscribe
// a user. Suppression happens only on a POST: the confirmation form (/confirm)
// and the RFC 8058 one-click. A resubscribe POST removes the marketing
// suppression. The token is the capability, so no auth is required.
type EmailHandler struct {
	suppressions repository.EmailSuppressionRepository
	linkSecret   SecretResolver
}

// NewEmailHandler constructs the unsubscribe handler.
func NewEmailHandler(suppressions repository.EmailSuppressionRepository, linkSecret SecretResolver) *EmailHandler {
	return &EmailHandler{suppressions: suppressions, linkSecret: linkSecret}
}

// Register mounts the public routes (no auth — token-gated).
func (h *EmailHandler) Register(app *fiber.App) {
	app.Get("/api/email/unsubscribe", h.Unsubscribe)
	app.Post("/api/email/unsubscribe/confirm", h.UnsubscribeConfirm)
	app.Post("/api/email/unsubscribe", h.UnsubscribeOneClick)
	app.Post("/api/email/resubscribe", h.Resubscribe)
}

// Unsubscribe godoc
// @Summary      Unsubscribe landing (link)
// @Description  Verifies a signed token and renders a confirmation page.
// @Description  Idempotent — GET never suppresses (so prefetchers / mail
// @Description  scanners can't unsubscribe a user); the form POSTs to /confirm.
// @Tags         email
// @Produce      html
// @Param        token  query  string  true  "Signed unsubscribe token"
// @Success      200  {string}  string  "Confirmation page"
// @Failure      400  {string}  string  "Invalid or expired token"
// @Router       /api/email/unsubscribe [get]
func (h *EmailHandler) Unsubscribe(c *fiber.Ctx) error {
	if _, err := h.resolve(c); err != nil {
		if isTokenError(err) {
			return c.Status(fiber.StatusBadRequest).Type("html").SendString(unsubscribeInvalidPage())
		}
		return err // secret-read / server error → 500
	}
	return c.Status(fiber.StatusOK).Type("html").SendString(unsubscribeConfirmPage(tokenFrom(c)))
}

// UnsubscribeConfirm godoc
// @Summary      Confirm unsubscribe (form POST)
// @Description  Verifies the token, suppresses the address from marketing mail,
// @Description  and renders the success page (with a resubscribe option).
// @Tags         email
// @Accept       x-www-form-urlencoded
// @Produce      html
// @Param        token  formData  string  true  "Signed unsubscribe token"
// @Success      200  {string}  string  "Unsubscribed page"
// @Failure      400  {string}  string  "Invalid or expired token"
// @Router       /api/email/unsubscribe/confirm [post]
func (h *EmailHandler) UnsubscribeConfirm(c *fiber.Ctx) error {
	addr, err := h.resolve(c)
	if isTokenError(err) {
		return c.Status(fiber.StatusBadRequest).Type("html").SendString(unsubscribeInvalidPage())
	}
	if err != nil {
		return err // secret-read / server error → 500
	}
	if err := h.suppress(reqCtx(c), addr); err != nil {
		return err
	}
	return c.Status(fiber.StatusOK).Type("html").SendString(unsubscribeOKPage(tokenFrom(c)))
}

// UnsubscribeOneClick godoc
// @Summary      One-click unsubscribe (RFC 8058)
// @Description  The POST target advertised in the List-Unsubscribe-Post header.
// @Tags         email
// @Param        token  query  string  false  "Signed unsubscribe token"
// @Success      200  "Unsubscribed"
// @Failure      400  {object}  map[string]string
// @Router       /api/email/unsubscribe [post]
func (h *EmailHandler) UnsubscribeOneClick(c *fiber.Ctx) error {
	addr, err := h.resolve(c)
	if isTokenError(err) {
		return fiber.NewError(fiber.StatusBadRequest, "invalid or expired token")
	}
	if err != nil {
		return err
	}
	if err := h.suppress(reqCtx(c), addr); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusOK)
}

// Resubscribe godoc
// @Summary      Resubscribe to marketing email
// @Description  Verifies the token and removes the marketing suppression for
// @Description  the address (re-opt-in). Any hard-bounce/complaint suppression
// @Description  is left in place.
// @Tags         email
// @Accept       x-www-form-urlencoded
// @Produce      html
// @Param        token  formData  string  true  "Signed unsubscribe token"
// @Success      200  {string}  string  "Resubscribed page"
// @Failure      400  {string}  string  "Invalid or expired token"
// @Router       /api/email/resubscribe [post]
func (h *EmailHandler) Resubscribe(c *fiber.Ctx) error {
	addr, err := h.resolve(c)
	if isTokenError(err) {
		return c.Status(fiber.StatusBadRequest).Type("html").SendString(unsubscribeInvalidPage())
	}
	if err != nil {
		return err
	}
	if err := h.suppressions.RemoveMarketing(reqCtx(c), addr); err != nil {
		return err
	}
	return c.Status(fiber.StatusOK).Type("html").SendString(resubscribeOKPage())
}

// tokenFrom extracts the signed token from the query or the form body.
func tokenFrom(c *fiber.Ctx) string {
	token := c.Query("token")
	if token == "" {
		token = c.FormValue("token")
	}
	return token
}

// isTokenError reports whether err is a bad/expired token (→ 400), as opposed to
// a server error (→ 500).
func isTokenError(err error) bool {
	return errors.Is(err, email.ErrInvalidToken) || errors.Is(err, email.ErrExpiredToken)
}

// resolve extracts the token and returns the address it authorises. A
// missing/forged/expired token yields email.ErrInvalidToken / ErrExpiredToken; a
// secret-read failure returns that error (mapped to 500).
func (h *EmailHandler) resolve(c *fiber.Ctx) (string, error) {
	token := tokenFrom(c)
	if token == "" {
		return "", email.ErrInvalidToken
	}
	secret, err := h.linkSecret(reqCtx(c))
	if err != nil {
		return "", err
	}
	if secret == "" {
		return "", email.ErrInvalidToken
	}
	return email.VerifyUnsubscribe(secret, token, time.Now().UTC())
}

func (h *EmailHandler) suppress(ctx context.Context, addr string) error {
	id, err := models.NewID()
	if err != nil {
		return err
	}
	return h.suppressions.Upsert(ctx, &models.EmailSuppression{
		ID:     id,
		Email:  repository.NormalizeEmail(addr),
		Scope:  models.EmailSuppressionScopeMarketing,
		Reason: models.EmailSuppressionReasonUnsubscribe,
		Source: models.EmailSuppressionSourceUser,
	})
}

// --- Branded pages (Ogen / Harbor) ---------------------------------------
//
// Server-rendered so the flow works regardless of SPA availability, matching
// the email design: warm beige page, white hairline card, near-black ink,
// black uppercase CTA, Zalando type with a system fallback.

const emailPageFonts = `<link rel="preconnect" href="https://fonts.googleapis.com">` +
	`<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>` +
	`<link href="https://fonts.googleapis.com/css2?family=Zalando+Sans:wght@400;600;700&family=Zalando+Sans+Expanded:wght@600;700&display=swap" rel="stylesheet">`

// brandedPage wraps a heading + body (+ optional action HTML) in the Ogen card
// layout. heading/title are HTML-escaped; body and action are trusted static
// markup built below (any token inside action is escaped at its call site).
func brandedPage(title, heading, body, action string) string {
	return `<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width, initial-scale=1">` +
		`<title>` + html.EscapeString(title) + ` · Ogen</title>` + emailPageFonts +
		`</head>` +
		`<body style="margin:0;background:#f2f1ef;font-family:'Zalando Sans',-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#151515;">` +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr><td align="center" style="padding:64px 20px;">` +
		`<table role="presentation" width="460" cellpadding="0" cellspacing="0" style="max-width:460px;width:100%;">` +
		`<tr><td style="padding:0 4px 16px;font-family:'Zalando Sans Expanded','Zalando Sans',sans-serif;font-size:18px;font-weight:700;color:#151515;">Ogen</td></tr>` +
		`<tr><td style="background:#ffffff;border:1px solid #e0deda;border-radius:4px;padding:40px 32px;">` +
		`<h1 style="margin:0 0 12px;font-family:'Zalando Sans Expanded','Zalando Sans',sans-serif;font-size:22px;line-height:28px;font-weight:700;color:#151515;">` + html.EscapeString(heading) + `</h1>` +
		`<p style="margin:0;font-size:15px;line-height:24px;color:#57534d;">` + body + `</p>` +
		action +
		`</td></tr></table></td></tr></table></body></html>`
}

// brandButton is the black, uppercase, sharp-cornered CTA used on the pages.
func brandButton(label string) string {
	return `<button type="submit" style="margin-top:24px;background:#151515;color:#ffffff;border:0;border-radius:3px;` +
		`padding:12px 22px;font:600 13px/1 'Zalando Sans',-apple-system,sans-serif;letter-spacing:.05em;` +
		`text-transform:uppercase;cursor:pointer;">` + html.EscapeString(label) + `</button>`
}

func tokenForm(action, token, buttonLabel string) string {
	return `<form method="post" action="` + action + `">` +
		`<input type="hidden" name="token" value="` + html.EscapeString(token) + `">` +
		brandButton(buttonLabel) + `</form>`
}

func unsubscribeConfirmPage(token string) string {
	return brandedPage("Unsubscribe", "Unsubscribe from marketing emails?",
		"You'll stop receiving marketing emails from Ogen. Essential account emails may still be sent.",
		tokenForm("/api/email/unsubscribe/confirm", token, "Unsubscribe"))
}

func unsubscribeOKPage(token string) string {
	return brandedPage("Unsubscribed", "You're unsubscribed",
		"You'll no longer receive marketing emails from Ogen. Changed your mind?",
		tokenForm("/api/email/resubscribe", token, "Resubscribe"))
}

func resubscribeOKPage() string {
	return brandedPage("Resubscribed", "You're resubscribed",
		"You'll receive marketing emails from Ogen again. You can unsubscribe any time from the link in each email.",
		"")
}

func unsubscribeInvalidPage() string {
	return brandedPage("Invalid link", "This link is no longer valid",
		"The unsubscribe link is invalid or has expired. Please use the link in a more recent email.",
		"")
}
