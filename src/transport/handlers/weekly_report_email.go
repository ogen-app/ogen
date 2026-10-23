package handlers

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/infra/email"
)

// WeeklyReportPrefs reads and sets a membership's weekly-report opt-out.
type WeeklyReportPrefs interface {
	OptedOut(ctx context.Context, userID string) (bool, error)
	SetOptOut(ctx context.Context, userID string, optOut bool) error
}

// WeeklyReportEmailHandler serves the weekly report's opt-out: the public,
// token-gated unsubscribe pages linked from the email, and the signed-in
// preference endpoint behind the settings toggle. Like the marketing
// unsubscribe, the link (GET) only renders a confirmation page, so a link
// prefetcher can't turn the report off; the change happens on a POST.
type WeeklyReportEmailHandler struct {
	prefs      WeeklyReportPrefs
	linkSecret SecretResolver
	auth       fiber.Handler
}

// NewWeeklyReportEmailHandler wires the handler.
func NewWeeklyReportEmailHandler(prefs WeeklyReportPrefs, linkSecret SecretResolver, auth fiber.Handler) *WeeklyReportEmailHandler {
	return &WeeklyReportEmailHandler{prefs: prefs, linkSecret: linkSecret, auth: auth}
}

// Register mounts the routes. The unsubscribe routes are public (the token is
// the capability); the preference routes need a session.
func (h *WeeklyReportEmailHandler) Register(app *fiber.App) {
	app.Get("/api/email/weekly-report/unsubscribe", h.Unsubscribe)
	app.Post("/api/email/weekly-report/unsubscribe/confirm", h.UnsubscribeConfirm)
	app.Post("/api/email/weekly-report/unsubscribe", h.UnsubscribeOneClick)
	app.Post("/api/email/weekly-report/resubscribe", h.Resubscribe)
	app.Get("/api/me/email-preferences", h.auth, h.GetPreferences)
	app.Patch("/api/me/email-preferences", h.auth, h.UpdatePreferences)
}

// emailPreferences is the signed-in member's email settings for the active
// workspace.
type emailPreferences struct {
	WeeklyReport bool `json:"weekly_report"`
}

// emailPreferencesPatch is the PATCH body; an omitted field is left as is.
type emailPreferencesPatch struct {
	WeeklyReport *bool `json:"weekly_report"`
}

// Unsubscribe godoc
// @Summary      Weekly report unsubscribe landing (link)
// @Description  Verifies the signed token and renders a confirmation page. GET never changes anything.
// @Tags         email
// @Produce      html
// @Param        token  query  string  true  "Signed weekly-report token"
// @Success      200  {string}  string  "Confirmation page"
// @Failure      400  {string}  string  "Invalid or expired token"
// @Router       /api/email/weekly-report/unsubscribe [get]
func (h *WeeklyReportEmailHandler) Unsubscribe(c *fiber.Ctx) error {
	if _, err := h.resolve(c); err != nil {
		return h.pageError(c, err)
	}
	return c.Type("html").SendString(brandedPage("Unsubscribe", "Stop the weekly report?",
		"You'll stop getting the Monday report for this workspace. Your other workspaces and account emails aren't affected.",
		tokenForm("/api/email/weekly-report/unsubscribe/confirm", tokenFrom(c), "Unsubscribe")))
}

// UnsubscribeConfirm godoc
// @Summary      Confirm weekly report unsubscribe (form POST)
// @Tags         email
// @Accept       x-www-form-urlencoded
// @Produce      html
// @Param        token  formData  string  true  "Signed weekly-report token"
// @Success      200  {string}  string  "Unsubscribed page"
// @Failure      400  {string}  string  "Invalid or expired token"
// @Router       /api/email/weekly-report/unsubscribe/confirm [post]
func (h *WeeklyReportEmailHandler) UnsubscribeConfirm(c *fiber.Ctx) error {
	if err := h.set(c, true); err != nil {
		return h.pageError(c, err)
	}
	return c.Type("html").SendString(brandedPage("Unsubscribed", "The weekly report is off",
		"You won't get the Monday report for this workspace any more. Changed your mind?",
		tokenForm("/api/email/weekly-report/resubscribe", tokenFrom(c), "Turn it back on")))
}

// UnsubscribeOneClick godoc
// @Summary      Weekly report one-click unsubscribe (RFC 8058)
// @Description  The POST target advertised in the report's List-Unsubscribe-Post header.
// @Tags         email
// @Produce      json
// @Param        token  query  string  true  "Signed weekly-report token"
// @Success      200  {object}  emailPreferences
// @Failure      400  {object}  map[string]string
// @Router       /api/email/weekly-report/unsubscribe [post]
func (h *WeeklyReportEmailHandler) UnsubscribeOneClick(c *fiber.Ctx) error {
	if err := h.set(c, true); err != nil {
		return tokenErrorJSON(err)
	}
	return c.JSON(emailPreferences{WeeklyReport: false})
}

// Resubscribe godoc
// @Summary      Turn the weekly report back on
// @Tags         email
// @Accept       x-www-form-urlencoded
// @Produce      html
// @Param        token  formData  string  true  "Signed weekly-report token"
// @Success      200  {string}  string  "Resubscribed page"
// @Failure      400  {string}  string  "Invalid or expired token"
// @Router       /api/email/weekly-report/resubscribe [post]
func (h *WeeklyReportEmailHandler) Resubscribe(c *fiber.Ctx) error {
	if err := h.set(c, false); err != nil {
		return h.pageError(c, err)
	}
	return c.Type("html").SendString(brandedPage("Resubscribed", "The weekly report is back on",
		"You'll get the report for this workspace every Monday at 9:00 your workspace time.", ""))
}

// GetPreferences godoc
// @Summary      Email preferences
// @Description  The signed-in member's email preferences for the active workspace.
// @Tags         email
// @Produce      json
// @Security     CookieAuth
// @Success      200  {object}  emailPreferences
// @Failure      401  {object}  map[string]string
// @Router       /api/me/email-preferences [get]
func (h *WeeklyReportEmailHandler) GetPreferences(c *fiber.Ctx) error {
	s, err := sessionFrom(c)
	if err != nil {
		return err
	}
	out, err := h.prefs.OptedOut(reqCtx(c), s.UserID)
	if err != nil {
		return notFound(err, "membership not found")
	}
	return c.JSON(emailPreferences{WeeklyReport: !out})
}

// UpdatePreferences godoc
// @Summary      Update email preferences
// @Description  Turns the weekly report on or off for the signed-in member in the active workspace.
// @Tags         email
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body  emailPreferencesPatch  true  "Preferences to change"
// @Success      200  {object}  emailPreferences
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Router       /api/me/email-preferences [patch]
func (h *WeeklyReportEmailHandler) UpdatePreferences(c *fiber.Ctx) error {
	s, err := sessionFrom(c)
	if err != nil {
		return err
	}
	var body emailPreferencesPatch
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	ctx := reqCtx(c)
	if body.WeeklyReport != nil {
		if err := h.prefs.SetOptOut(ctx, s.UserID, !*body.WeeklyReport); err != nil {
			return notFound(err, "membership not found")
		}
	}
	out, err := h.prefs.OptedOut(ctx, s.UserID)
	if err != nil {
		return notFound(err, "membership not found")
	}
	return c.JSON(emailPreferences{WeeklyReport: !out})
}

// resolve verifies the token and returns the membership it names.
func (h *WeeklyReportEmailHandler) resolve(c *fiber.Ctx) (string, error) {
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
	return email.VerifyScoped(secret, email.ScopeWeeklyReport, token, time.Now().UTC())
}

// set verifies the token and sets the membership's opt-out. A membership that
// no longer exists reads as an invalid link.
func (h *WeeklyReportEmailHandler) set(c *fiber.Ctx, optOut bool) error {
	userID, err := h.resolve(c)
	if err != nil {
		return err
	}
	if err := h.prefs.SetOptOut(reqCtx(c), userID, optOut); errors.Is(err, sql.ErrNoRows) {
		return email.ErrInvalidToken
	} else if err != nil {
		return err
	}
	return nil
}

func (h *WeeklyReportEmailHandler) pageError(c *fiber.Ctx, err error) error {
	if isTokenError(err) {
		return c.Status(fiber.StatusBadRequest).Type("html").SendString(brandedPage("Invalid link",
			"This link is no longer valid",
			"The link is invalid or has expired. Use the link in a more recent weekly report.", ""))
	}
	return err
}

func tokenErrorJSON(err error) error {
	if isTokenError(err) {
		return fiber.NewError(fiber.StatusBadRequest, "invalid or expired token")
	}
	return err
}
