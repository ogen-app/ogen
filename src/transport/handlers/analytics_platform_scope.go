package handlers

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
)

// platformScope is the set of platforms an analytics read is narrowed to. A
// nil scope means every platform. Analytics rows carry only the platform's
// display name, which operators can rename, so rows are matched through their
// post's platform_id and follower snapshots through the Zernio slug.
type platformScope struct {
	ids   map[string]bool
	slugs map[string]bool
}

// parsePlatformScope reads the repeatable `platform` query param (Zernio slugs;
// comma-separated values are also accepted). Absent yields a nil scope. Any
// slug that matches no platform is a 400 so a typo in a saved link fails
// loudly instead of silently widening to every platform.
func (h *AnalyticsHandler) parsePlatformScope(c *fiber.Ctx) (*platformScope, error) {
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
	s := &platformScope{ids: map[string]bool{}, slugs: map[string]bool{}}
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

func (s *platformScope) platformIDs() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	return out
}

func (s *platformScope) allowsPlatformID(id string) bool {
	return s == nil || s.ids[id]
}

func (s *platformScope) allowsSlug(slug string) bool {
	return s == nil || s.slugs[strings.ToLower(slug)]
}

// filterRows keeps the analytics rows whose post is on an in-scope platform.
func (h *AnalyticsHandler) filterRows(ctx context.Context, s *platformScope, sets ...*[]models.PostAnalytics) error {
	if s == nil {
		return nil
	}
	var ids []string
	for _, set := range sets {
		for i := range *set {
			ids = append(ids, (*set)[i].PostID)
		}
	}
	platformOf := map[string]string{}
	if h.posts != nil {
		var err error
		if platformOf, err = h.posts.PlatformIDsByID(ctx, ids); err != nil {
			return err
		}
	}
	for _, set := range sets {
		kept := (*set)[:0:0]
		for _, r := range *set {
			if id, ok := platformOf[r.PostID]; ok && s.allowsPlatformID(id) {
				kept = append(kept, r)
			}
		}
		*set = kept
	}
	return nil
}
