package handlers

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/usecase/analytics"
)

// analyticsScope is what an analytics read is narrowed to; see
// analytics.Scope.
type analyticsScope = analytics.Scope

// Service is the analytics reads over the handler's stores, with its cached
// baselines. The assistant reads through it too, so both share the caches.
func (h *AnalyticsHandler) Service() *analytics.Service {
	return &analytics.Service{
		Repo: h.repo, Followers: h.followerRepo, Posts: h.posts, Platforms: h.platforms,
		Accounts: h.accounts, Campaigns: h.campaigns, ReachSamples: h.reachByAgeSamples,
		LifespanSamples: h.lifespanSamples, Zernio: h.client, ProfileID: h.profileID, InsightCache: h.insights.get,
	}
}

// parseScope reads both the `platform` and the `campaign_id` query params.
func (h *AnalyticsHandler) parseScope(c *fiber.Ctx) (*analyticsScope, error) {
	return h.scope(c, strings.TrimSpace(c.Query("campaign_id")))
}

// parsePlatformScope reads the repeatable `platform` query param only.
func (h *AnalyticsHandler) parsePlatformScope(c *fiber.Ctx) (*analyticsScope, error) {
	return h.scope(c, "")
}

// scope resolves the `platform` slugs (repeatable; comma-separated values
// are also accepted) and a campaign. An unknown slug is a 400, so a typo in
// a saved link fails loudly; an unknown or foreign campaign is a 404.
func (h *AnalyticsHandler) scope(c *fiber.Ctx, campaignID string) (*analyticsScope, error) {
	var slugs []string
	for _, raw := range c.Context().QueryArgs().PeekMulti("platform") {
		for v := range strings.SplitSeq(string(raw), ",") {
			if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
				slugs = append(slugs, v)
			}
		}
	}
	s, err := h.Service().Scope(reqCtx(c), slugs, campaignID)
	switch {
	case errors.Is(err, analytics.ErrInvalidPlatform):
		return nil, fiber.NewError(fiber.StatusBadRequest, "invalid_platform")
	case errors.Is(err, analytics.ErrCampaignNotFound):
		return nil, fiber.NewError(fiber.StatusNotFound, "campaign not found")
	}
	return s, err
}
