package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/usecase/analytics"
)

// PostDetail serves the CON-250 per-post statistics drill-down: the post header,
// six metric cards scored against the account's typical post at the same age
// (a per-metric p25/p50/p75 usual-range band plus an against-typical multiplier),
// the per-metric running-total series since publishing, and a deterministic
// narrative wired to the CON-239 lifespan. It reads one post's current row +
// snapshot trajectory and the shared per-platform baseline / lifespan curves;
// there is no window parameter (the series spans [published_at, now]).
//
// It is distinct from GET /api/posts/:id/analytics, which stays the lean
// raw-snapshot/pending endpoint. A post the refresh hasn't recorded yet returns
// available:true with an awaiting_platform overview rather than a pending stub.
//
// PostDetail godoc
// @Summary      Per-post statistics
// @Description  Header + six metric cards (against-typical multiplier + usual-range band) + per-metric running-total series + narrative for one published post. Distinct from the lean /api/posts/{id}/analytics snapshot.
// @Tags         analytics
// @Produce      json
// @Security     CookieAuth
// @Param        post_id  path  string  true  "Post id"
// @Success      200  {object}  handlers.insightEnvelope
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Router       /api/analytics/posts/{post_id} [get]
func (h *AnalyticsHandler) PostDetail(c *fiber.Ctx) error {
	if h.repo == nil {
		return c.JSON(insightEnvelope{Available: false, Reason: reasonNotConfigured})
	}
	resp, reason, err := h.service().PostStats(reqCtx(c), c.Params("post_id"))
	switch {
	case errors.Is(err, analytics.ErrPostNotFound):
		return fiber.NewError(fiber.StatusNotFound, "post not found")
	case errors.Is(err, analytics.ErrNotPublishedViaPublisher):
		// Analytics is defined only for posts published through a publisher;
		// mirror the CON-93 machine-readable 409 so the client can tell "wrong
		// kind of post" from "not found".
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"code":  "not_published_via_publisher",
			"error": "post has not been published through a publisher",
		})
	case err != nil:
		return err
	}
	if resp == nil {
		return c.JSON(insightEnvelope{Available: false, Reason: reason})
	}
	return c.JSON(insightEnvelope{Available: true, Data: *resp})
}
