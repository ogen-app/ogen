package handlers

import (
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// PluginRoutePrefix is where every design-tool plugin route lives. Those
// routes take bearer plugin tokens and answer CORS for any origin (a plugin
// UI runs in an iframe whose origin is "null"), so the credentialed UI CORS
// policy must skip this prefix.
const PluginRoutePrefix = "/api/plugins"

// CodePluginTokenInvalid is the 401 code for every plugin-token failure. The
// plugin treats it as "connect again"; the cause stays server-side.
const CodePluginTokenInvalid = "plugin_token_invalid"

// pluginTokenLocal is the c.Locals key holding the authenticating
// *models.PluginToken.
const pluginTokenLocal = "plugin_token"

// RequirePluginToken authenticates a plugin request by its
// "Authorization: Bearer ogp_…" token. Cookies are ignored, and a plugin token
// is never accepted by RequireAuth, so each credential reaches only its own
// routes.
//
// Every request re-resolves the token's membership through GetMembership, the
// same chokepoint RequireAuth uses: a revoked token, a removed member and a
// suspended or deleted workspace are all refused with the one 401 code. On
// success it sets the same locals as RequireAuth (session, tenant, user id),
// so handlers built for cookie sessions run unchanged.
func RequirePluginToken(tokens repository.PluginTokenRepository, users repository.UserRepository) fiber.Handler {
	return func(c *fiber.Ctx) error {
		raw, ok := bearerToken(c.Get(fiber.HeaderAuthorization))
		if !ok || !models.LooksLikePluginToken(raw) {
			return rejectPluginToken(c)
		}
		ctx := reqCtx(c)
		token, err := tokens.GetActiveByHash(ctx, models.HashPluginSecret(raw))
		if errors.Is(err, sql.ErrNoRows) {
			return rejectPluginToken(c)
		}
		if err != nil {
			return err
		}
		membership, err := users.GetMembership(ctx, token.AccountID, token.TenantID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && membership.ID != token.UserID) {
			return rejectPluginToken(c)
		}
		if err != nil {
			return err
		}

		c.Locals(pluginTokenLocal, token)
		c.Locals("session", &models.Session{
			AccountID: token.AccountID,
			UserID:    membership.ID,
			TenantID:  membership.TenantID,
		})
		c.Locals(tenantctx.Key, membership.TenantID)
		c.Locals(logging.UserIDKey, membership.ID)
		c.Locals(logging.PluginTokenIDKey, token.ID)

		touchPluginToken(c, tokens, token)
		return c.Next()
	}
}

// touchPluginToken records the token's use, skipping the write while the
// stored stamp is recent. A failed write is logged, never fatal.
func touchPluginToken(c *fiber.Ctx, tokens repository.PluginTokenRepository, token *models.PluginToken) {
	now := time.Now().UTC()
	if token.LastUsedAt != nil && now.Sub(*token.LastUsedAt) < repository.PluginTokenTouchInterval {
		return
	}
	if err := tokens.TouchLastUsed(reqCtx(c), token.ID, now); err != nil {
		slog.WarnContext(reqCtx(c), "plugin token last_used_at write failed",
			logging.AttrComponent, "plugins", logging.AttrError, err)
	}
}

// bearerToken extracts the credential from an Authorization header value.
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func rejectPluginToken(c *fiber.Ctx) error {
	return rejectCode(c, fiber.StatusUnauthorized, CodePluginTokenInvalid, "plugin token is missing, revoked or no longer valid")
}
