package handlers

import (
	"context"
	"errors"
	"maps"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/signupaccess"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// Tester-invite preview throttle: public, so it is budgeted per client IP.
const (
	tenantInvitePreviewPerIP = 30
	tenantInviteRateWindow   = time.Hour
)

// tenantInviteInvalidMsg is the one answer for every unusable tester invite,
// so neither the preview nor the signup is an oracle for tokens.
const tenantInviteInvalidMsg = "This invite is invalid or has expired. Ask the Ogen team for a new one."

// SignupAccessService is the signup-access surface the public routes read.
type SignupAccessService interface {
	Settings(ctx context.Context) (models.SignupSettings, error)
	Preview(ctx context.Context, token string) (signupaccess.Preview, error)
}

// SignupAccessHandler serves what the signup page needs before a form is
// shown: whether self-registration is open, and what a tester invite link is
// for.
type SignupAccessHandler struct {
	svc       SignupAccessService
	ipLimiter *keyedRateLimiter
}

// NewSignupAccessHandler builds the handler.
func NewSignupAccessHandler(svc SignupAccessService) *SignupAccessHandler {
	return &SignupAccessHandler{svc: svc, ipLimiter: newKeyedRateLimiter(tenantInvitePreviewPerIP, tenantInviteRateWindow)}
}

// Register mounts the public routes.
func (h *SignupAccessHandler) Register(app *fiber.App) {
	app.Get("/api/signup-config", h.Config)
	app.Get("/api/tenant-invites/:token", h.Preview)
}

type signupConfigResponse struct {
	Mode string `json:"mode" enums:"open,invite_only"`
}

// Config godoc
// @Summary      Signup mode
// @Description  Public. open: anyone may sign up. invite_only: only tester invites create workspaces, and the signup page shows a private-beta screen instead of the form. Tester invite links work in both modes. Cacheable for 30 seconds.
// @Tags         tenants
// @Produce      json
// @Success      200  {object}  signupConfigResponse
// @Router       /api/signup-config [get]
func (h *SignupAccessHandler) Config(c *fiber.Ctx) error {
	st, err := h.svc.Settings(reqCtx(c))
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderCacheControl, "public, max-age=30")
	return c.JSON(signupConfigResponse{Mode: st.Mode})
}

// Preview godoc
// @Summary      Preview a tester invite
// @Description  Public. For a usable tester invite: the invited address (the signup form locks it) and when the link expires. Every unusable token (unknown, expired, revoked, redeemed) answers the same 410. Throttled per IP. Never reveals the tier, groups, note or inviter.
// @Tags         tenants
// @Produce      json
// @Param        token  path  string  true  "Invite token"
// @Success      200  {object}  signupaccess.Preview
// @Failure      410  {object}  map[string]string
// @Failure      429  {object}  map[string]string
// @Router       /api/tenant-invites/{token} [get]
func (h *SignupAccessHandler) Preview(c *fiber.Ctx) error {
	if ok, retry := h.ipLimiter.allow(c.IP()); !ok {
		return tooManyRequests(c, retry, inviteThrottledMsg)
	}
	p, err := h.svc.Preview(reqCtx(c), c.Params("token"))
	if errors.Is(err, signupaccess.ErrInviteInvalid) {
		return fiber.NewError(fiber.StatusGone, tenantInviteInvalidMsg)
	}
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(p)
}

// signupAccessError renders the signup gate and tester-invite refusals.
// handled is false for any other error.
func signupAccessError(c *fiber.Ctx, err error) (handled bool, _ error) {
	switch {
	case errors.Is(err, signup.ErrInviteRequired):
		return true, rejectCode(c, fiber.StatusForbidden, "invite_required", "Ogen is in private beta. Sign up with the link from your invitation email.")
	case errors.Is(err, signup.ErrInviteEmailMismatch):
		return true, rejectCode(c, fiber.StatusForbidden, "invite_email_mismatch", "This invite is for a different email address. Sign up with the address it was sent to.")
	case errors.Is(err, signup.ErrInviteInvalid):
		return true, fiber.NewError(fiber.StatusGone, tenantInviteInvalidMsg)
	}
	return false, nil
}

// signupPayload is the signup activity payload: the redeemed invite, when
// there was one, plus extra.
func signupPayload(res *signup.Result, extra map[string]any) map[string]any {
	out := map[string]any{}
	maps.Copy(out, extra)
	if res.Invite != nil {
		out["invite_id"] = res.Invite.ID
		out["complimentary"] = true
	}
	return out
}
