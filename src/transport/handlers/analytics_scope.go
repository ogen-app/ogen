package handlers

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// analyticsScope is what an analytics read is narrowed to: a set of platforms
// and/or one campaign. A nil scope means the whole workspace; a nil platform set
// means every platform and an empty campaignID every campaign. Analytics rows
// carry only the platform's display name, which operators can rename, and no
// campaign at all, so rows are matched through their post's platform_id and
// campaign_id, and follower snapshots through the Zernio slug.
type analyticsScope struct {
	ids        map[string]bool
	slugs      map[string]bool
	campaignID string
}

// parseScope reads both the `platform` and the `campaign_id` query params.
func (h *AnalyticsHandler) parseScope(c *fiber.Ctx) (*analyticsScope, error) {
	s, err := h.parsePlatformScope(c)
	if err != nil {
		return nil, err
	}
	campaignID := strings.TrimSpace(c.Query("campaign_id"))
	if campaignID == "" {
		return s, nil
	}
	// The tenant hook makes a foreign campaign indistinguishable from an
	// unknown one, so both 404.
	if h.campaigns == nil {
		return nil, fiber.NewError(fiber.StatusNotFound, "campaign not found")
	}
	if _, err := h.campaigns.GetByID(reqCtx(c), campaignID); err != nil {
		return nil, notFound(err, "campaign not found")
	}
	if s == nil {
		s = &analyticsScope{}
	}
	s.campaignID = campaignID
	return s, nil
}

// parsePlatformScope reads the repeatable `platform` query param (Zernio slugs;
// comma-separated values are also accepted). Absent yields a nil scope. Any
// slug that matches no platform is a 400 so a typo in a saved link fails
// loudly instead of silently widening to every platform.
func (h *AnalyticsHandler) parsePlatformScope(c *fiber.Ctx) (*analyticsScope, error) {
	var slugs []string
	for _, raw := range c.Context().QueryArgs().PeekMulti("platform") {
		for v := range strings.SplitSeq(string(raw), ",") {
			if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
				slugs = append(slugs, v)
			}
		}
	}
	if len(slugs) == 0 {
		return nil, nil
	}
	bySlug, err := h.platformsBySlug(reqCtx(c))
	if err != nil {
		return nil, err
	}
	s := &analyticsScope{ids: map[string]bool{}, slugs: map[string]bool{}}
	for _, slug := range slugs {
		p, ok := bySlug[slug]
		if !ok {
			return nil, fiber.NewError(fiber.StatusBadRequest, "invalid_platform")
		}
		s.ids[p.ID] = true
		s.slugs[slug] = true
	}
	return s, nil
}

// platformsBySlug indexes every platform, disabled ones included (their
// history is still real), by its lower-cased Zernio slug.
func (h *AnalyticsHandler) platformsBySlug(ctx context.Context) (map[string]models.Platform, error) {
	out := map[string]models.Platform{}
	if h.platforms == nil {
		return out, nil
	}
	plats, err := h.platforms.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range plats {
		if p.ZernioID != "" {
			out[strings.ToLower(p.ZernioID)] = p
		}
	}
	return out, nil
}

func (s *analyticsScope) platformIDs() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	return out
}

func (s *analyticsScope) campaign() string {
	if s == nil {
		return ""
	}
	return s.campaignID
}

// narrowsPlatforms reports whether the scope excludes any platform.
func (s *analyticsScope) narrowsPlatforms() bool {
	return s != nil && s.ids != nil
}

func (s *analyticsScope) allowsPlatformID(id string) bool {
	return !s.narrowsPlatforms() || s.ids[id]
}

func (s *analyticsScope) allowsSlug(slug string) bool {
	return !s.narrowsPlatforms() || s.slugs[strings.ToLower(slug)]
}

func (s *analyticsScope) allowsCampaign(id string) bool {
	return s.campaign() == "" || s.campaignID == id
}

// filterRows keeps the analytics rows whose post is in scope.
func (h *AnalyticsHandler) filterRows(ctx context.Context, s *analyticsScope, sets ...*[]models.PostAnalytics) error {
	if s == nil {
		return nil
	}
	var ids []string
	for _, set := range sets {
		for i := range *set {
			ids = append(ids, (*set)[i].PostID)
		}
	}
	keys := map[string]repository.PostScopeKey{}
	if h.posts != nil {
		var err error
		if keys, err = h.posts.ScopeKeysByID(ctx, ids); err != nil {
			return err
		}
	}
	for _, set := range sets {
		kept := (*set)[:0:0]
		for _, r := range *set {
			if k, ok := keys[r.PostID]; ok && s.allowsPlatformID(k.PlatformID) && s.allowsCampaign(k.CampaignID) {
				kept = append(kept, r)
			}
		}
		*set = kept
	}
	return nil
}
