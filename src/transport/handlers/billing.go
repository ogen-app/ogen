package handlers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/billing"
)

// maxBillingWebhookBytes bounds a webhook body; Lemon Squeezy's are a few KB.
const maxBillingWebhookBytes = 1 << 20

// BillingService is the billing usecase surface the HTTP handlers use.
type BillingService interface {
	Enabled() bool
	Billing(ctx context.Context, tenantID string) (billing.BillingView, error)
	Portal(ctx context.Context, tenantID string) (string, time.Time, error)
	ChangePlan(ctx context.Context, req billing.PlanRequest) (billing.PlanResult, error)
	HandleWebhook(ctx context.Context, signature string, body []byte) error
}

// EntitlementsReader renders the entitlements read a 200 plan change answers
// with. PricingHandler implements it.
type EntitlementsReader interface {
	entitlementsBody(c *fiber.Ctx, tenantID, accountID string) (any, error)
}

// BillingHandler serves the owner's billing card and portal link, the plan
// change, and the provider webhook.
type BillingHandler struct {
	svc      BillingService
	users    repository.UserRepository
	accounts repository.AccountRepository
	reader   EntitlementsReader
	auth     fiber.Handler
}

// NewBillingHandler builds the handler.
func NewBillingHandler(svc BillingService, users repository.UserRepository, accounts repository.AccountRepository, reader EntitlementsReader, auth fiber.Handler) *BillingHandler {
	return &BillingHandler{svc: svc, users: users, accounts: accounts, reader: reader, auth: auth}
}

// Register mounts the routes.
func (h *BillingHandler) Register(app *fiber.App) {
	app.Get("/api/billing", h.auth, h.Get)
	app.Post("/api/billing/portal", h.auth, h.Portal)
	app.Post("/api/workspace/plan", h.auth, h.ChangePlan)
	app.Post("/api/webhooks/lemonsqueezy", h.Webhook)
}

// billingError maps usecase errors to the contract's error codes.
func billingError(err error) error {
	switch {
	case errors.Is(err, billing.ErrUnavailable):
		return fiber.NewError(fiber.StatusServiceUnavailable, "billing_unavailable")
	case errors.Is(err, billing.ErrUnknownTier):
		return fiber.NewError(fiber.StatusBadRequest, "unknown_tier")
	case errors.Is(err, billing.ErrPriceMismatch):
		return fiber.NewError(fiber.StatusBadRequest, "price_mismatch")
	case errors.Is(err, billing.ErrTierNotAvailable):
		return fiber.NewError(fiber.StatusConflict, "tier_not_available")
	case errors.Is(err, billing.ErrNotChangeable):
		return fiber.NewError(fiber.StatusConflict, "subscription_not_changeable")
	case errors.Is(err, billing.ErrTrialChangeUnavailable):
		return fiber.NewError(fiber.StatusConflict, "trial_upgrade_unavailable")
	case errors.Is(err, billing.ErrNoSubscription):
		return fiber.NewError(fiber.StatusNotFound, "no_subscription")
	case errors.Is(err, billing.ErrTenantNotFound):
		return fiber.NewError(fiber.StatusNotFound, "workspace not found")
	case errors.Is(err, billing.ErrProvider):
		return fiber.NewError(fiber.StatusBadGateway, "provider_error")
	default:
		return err
	}
}

// Get godoc
// @Summary      Workspace billing
// @Description  The owner's billing card: the subscription in the provider's own status vocabulary, card brand and last four, and the net (VAT-exclusive) price. subscription is null when the workspace never bought; portal is false when there is no customer portal to open; test_mode marks the sandbox period.
// @Tags         billing
// @Produce      json
// @Security     CookieAuth
// @Success      200  {object}  billing.BillingView
// @Failure      403  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/billing [get]
func (h *BillingHandler) Get(c *fiber.Ctx) error {
	owner, err := requireOwner(c, h.users)
	if err != nil {
		return err
	}
	view, err := h.svc.Billing(reqCtx(c), owner.TenantID)
	if err != nil {
		return billingError(err)
	}
	return c.JSON(view)
}

type portalResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Portal godoc
// @Summary      Billing portal link
// @Description  A fresh, signed link to the provider's customer portal (card, invoices, cancellation). It expires within 24 hours; never cache it.
// @Tags         billing
// @Produce      json
// @Security     CookieAuth
// @Success      200  {object}  portalResponse
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      502  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/billing/portal [post]
func (h *BillingHandler) Portal(c *fiber.Ctx) error {
	owner, err := requireOwner(c, h.users)
	if err != nil {
		return err
	}
	link, expires, err := h.svc.Portal(reqCtx(c), owner.TenantID)
	if err != nil {
		return billingError(err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(portalResponse{URL: link, ExpiresAt: expires})
}

type changePlanRequest struct {
	TierID  string `json:"tier_id"`
	PriceID string `json:"price_id"`
}

type checkoutResponse struct {
	Checkout billing.Checkout `json:"checkout"`
}

// ChangePlan godoc
// @Summary      Choose the workspace plan
// @Description  A complimentary workspace (a tester's, complimentary=true on /api/me/entitlements) without a live subscription switches at once: 200 with the entitlements read, no checkout, no charge; it may pick any purchasable tier, and this works while billing is off. Otherwise, without a live subscription: 202 with a hosted checkout to redirect to (checkout.trial says whether the free month applies). With one: 200 with the entitlements read after the change. An upgrade lands now with proration; a downgrade is scheduled for the renewal (scheduled_change); choosing the current plan cancels a pending downgrade; a tier change during the free month ends it and charges now. tier_id is a tier or tier version id; price_id defaults to the monthly price.
// @Tags         billing
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      changePlanRequest  true  "Plan choice"
// @Success      200   {object}  myEntitlementsResponse
// @Success      202   {object}  checkoutResponse
// @Failure      400   {object}  map[string]string
// @Failure      403   {object}  map[string]string
// @Failure      409   {object}  map[string]string
// @Failure      502   {object}  map[string]string
// @Failure      503   {object}  map[string]string
// @Router       /api/workspace/plan [post]
func (h *BillingHandler) ChangePlan(c *fiber.Ctx) error {
	owner, err := requireOwner(c, h.users)
	if err != nil {
		return err
	}
	var req changePlanRequest
	if err := c.BodyParser(&req); err != nil || strings.TrimSpace(req.TierID) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "unknown_tier")
	}
	email := owner.Email
	if acc, err := h.accounts.GetByID(reqCtx(c), owner.AccountID); err == nil && acc.Email != "" {
		email = acc.Email
	}
	res, err := h.svc.ChangePlan(reqCtx(c), billing.PlanRequest{
		Buyer:   billing.Buyer{TenantID: owner.TenantID, UserID: owner.ID, AccountID: owner.AccountID, Email: email},
		TierID:  strings.TrimSpace(req.TierID),
		PriceID: strings.TrimSpace(req.PriceID),
	})
	if err != nil {
		return billingError(err)
	}
	if res.Checkout != nil {
		return c.Status(fiber.StatusAccepted).JSON(checkoutResponse{Checkout: *res.Checkout})
	}
	body, err := h.reader.entitlementsBody(c, owner.TenantID, owner.AccountID)
	if err != nil {
		return err
	}
	return c.JSON(body)
}

// Webhook godoc
// @Summary      Lemon Squeezy webhook
// @Description  Signature-verified (X-Signature, HMAC-SHA256 of the raw body) ingest of subscription and payment events. Redeliveries are recognised by body hash; snapshots older than the mirror are ignored; events from the other store mode are acknowledged and ignored. A failure answers 500 so the provider retries.
// @Tags         billing
// @Accept       json
// @Param        X-Signature  header  string  true  "HMAC-SHA256 hex of the body"
// @Success      200  "Acknowledged"
// @Failure      401  {object}  map[string]string
// @Failure      413  {object}  map[string]string
// @Failure      500  {object}  map[string]string  "processing failed; the provider retries"
// @Failure      503  {object}  map[string]string
// @Router       /api/webhooks/lemonsqueezy [post]
func (h *BillingHandler) Webhook(c *fiber.Ctx) error {
	if !h.svc.Enabled() {
		return fiber.NewError(fiber.StatusServiceUnavailable, "billing_unavailable")
	}
	raw := c.Body()
	if len(raw) > maxBillingWebhookBytes {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "payload too large")
	}
	// fasthttp reuses the body buffer after the handler returns; the usecase
	// keeps the payload, so it gets its own copy.
	body := bytes.Clone(raw)
	err := h.svc.HandleWebhook(reqCtx(c), c.Get("X-Signature"), body)
	switch {
	case err == nil:
		return c.SendStatus(fiber.StatusOK)
	case errors.Is(err, billing.ErrInvalidSignature):
		return fiber.NewError(fiber.StatusUnauthorized, "invalid signature")
	case errors.Is(err, billing.ErrUnavailable):
		return fiber.NewError(fiber.StatusServiceUnavailable, "billing_unavailable")
	default:
		slog.ErrorContext(reqCtx(c), "billing webhook failed; the provider will retry", logging.AttrComponent, "handlers.billing", logging.AttrError, err)
		return withCause(fiber.StatusInternalServerError, "webhook side effect failed", err)
	}
}
