package handlers

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/infra/repository"
)

// errBillingRequired is the billing gate's answer: the workspace must choose a
// plan and check out before it can be used.
var errBillingRequired = fiber.NewError(fiber.StatusPaymentRequired, "billing_required")

// BillingGate reports whether a workspace must check out before it can be used.
type BillingGate func(ctx context.Context, tenantID string) (bool, error)

// AuthOption tunes RequireAuth.
type AuthOption func(*authOptions)

type authOptions struct {
	billingGate BillingGate
}

// WithBillingGate enables the billing gate on the cookie chain. The gate is
// read with the session in one query; gate is only consulted when that
// query's membership fallback ran.
func WithBillingGate(gate BillingGate) AuthOption {
	return func(o *authOptions) { o.billingGate = gate }
}

// fallbackMembership resolves the account's membership when its default
// workspace is gone (deleted, or the membership removed): any live workspace,
// so the account isn't stranded on a dead default. 401 only when nothing
// remains. The fallback query reads no gate, so it is asked for separately.
func fallbackMembership(c *fiber.Ctx, userRepo repository.UserRepository, accountID string, gate BillingGate) (*repository.Membership, error) {
	user, err := userRepo.GetByAccountID(reqCtx(c), accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fiber.NewError(fiber.StatusUnauthorized, "no accessible workspace")
	}
	if err != nil {
		return nil, err
	}
	m := &repository.Membership{UserID: user.ID, TenantID: user.TenantID}
	if gate != nil {
		if m.BillingGated, err = gate(reqCtx(c), user.TenantID); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// billingAllowlist is every route a gated workspace still needs: signing in and
// out, booting the app, the workspace switcher and invitations, the plan and
// billing surface itself, and the event streams. Public and webhook routes do
// not pass through RequireAuth at all.
var billingAllowlist = []string{
	"/api/sessions",
	"/api/auth",
	"/api/account",
	"/api/current_user",
	"/api/settings",
	"/api/workspaces",
	"/api/tenants/current",
	"/api/invitations/accept",
	"/api/me/entitlements",
	"/api/workspace/plan",
	"/api/billing",
	"/api/events",
	"/api/notifications",
	"/api/announcements",
}

// billingAllowed reports whether a gated workspace may call a route: an
// allowlisted path or anything below it.
func billingAllowed(path string) bool {
	for _, p := range billingAllowlist {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// pluginBillingAllowed: a gated workspace's plugin may still identify itself
// and disconnect, so it can tell the designer what to do.
func pluginBillingAllowed(method, path string) bool {
	return strings.HasSuffix(path, "/me") || (method == fiber.MethodDelete && strings.HasSuffix(path, "/token"))
}
