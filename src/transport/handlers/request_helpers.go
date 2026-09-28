package handlers

import (
	"context"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// sessionFrom returns the session RequireAuth stored on the request, or a 401
// when the route was reached without one.
func sessionFrom(c *fiber.Ctx) (*models.Session, error) {
	s, ok := c.Locals("session").(*models.Session)
	if !ok || s == nil {
		return nil, fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	return s, nil
}

// actorID returns the authenticated user's id, or "" on an
// unauthenticated request. For attribution fields that tolerate a missing
// actor.
func actorID(c *fiber.Ctx) string {
	if s, ok := c.Locals("session").(*models.Session); ok && s != nil {
		return s.UserID
	}
	return ""
}

// load fetches the entity named by the :id route parameter, mapping a missing
// row to a 404 carrying notFoundMsg.
func load[T any](c *fiber.Ctx, fetch func(context.Context, string) (T, error), notFoundMsg string) (T, error) {
	return loadParam(c, "id", fetch, notFoundMsg)
}

// loadParam is load for an entity keyed by a route parameter other than :id.
func loadParam[T any](c *fiber.Ctx, param string, fetch func(context.Context, string) (T, error), notFoundMsg string) (T, error) {
	v, err := fetch(reqCtx(c), c.Params(param))
	if err != nil {
		var zero T
		return zero, notFound(err, notFoundMsg)
	}
	return v, nil
}

// quotaHold is a granted quota decision whose near-limit crossing is dispatched
// once the gated resource actually exists. The zero value (no tenant on the
// request) is a no-op.
type quotaHold struct {
	limiter  *entitlements.Limiter
	tenantID string
	decision entitlements.Decision
	held     bool
}

// requireQuota checks one more unit of featureKey for the request's tenant. A
// request without a tenant is not gated. The returned error is the limiter's
// 402/403 rejection.
func requireQuota(c *fiber.Ctx, limiter *entitlements.Limiter, featureKey string) (quotaHold, error) {
	return requireQuotaAmount(c, limiter, featureKey, 1)
}

// requireQuotaAmount is requireQuota for a create that consumes amount units
// at once (e.g. the byte size of an upload against media_storage_bytes).
func requireQuotaAmount(c *fiber.Ctx, limiter *entitlements.Limiter, featureKey string, amount int64) (quotaHold, error) {
	ctx := reqCtx(c)
	tenantID, ok := tenantctx.From(ctx)
	if !ok {
		return quotaHold{}, nil
	}
	dec, err := limiter.RequireAmount(ctx, tenantID, featureKey, amount)
	if err != nil {
		return quotaHold{}, err
	}
	return quotaHold{limiter: limiter, tenantID: tenantID, decision: dec, held: true}, nil
}

// dispatch fires the near-limit notification when this grant crossed the
// threshold. Call it only after the gated resource was created.
func (q quotaHold) dispatch(ctx context.Context) {
	if q.held {
		q.limiter.DispatchCrossing(ctx, q.tenantID, q.decision)
	}
}

// errPostSubmittedAttachments is the 409 for an attachment mutation on a post
// that is already scheduled or published.
const errPostSubmittedAttachments = "post has been submitted (scheduled or published) and its attachments are locked"

// ensureMutable rejects attachment mutations on a submitted post: Zernio
// snapshots attachments at schedule time, so a later change would silently
// diverge from what publishes.
func ensureMutable(post *models.Post) error {
	if post.Status.IsSubmitted() {
		return fiber.NewError(fiber.StatusConflict, errPostSubmittedAttachments)
	}
	return nil
}
