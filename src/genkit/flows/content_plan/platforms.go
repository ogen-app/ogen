package content_plan

import (
	"context"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func resolvePlatforms(ctx context.Context, targetPlatforms models.CampaignPlatforms, platformRepo repository.PlatformRepository) ([]resolvedPlatform, error) {
	all, err := platformRepo.List(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]models.Platform, len(all))
	for _, p := range all {
		byID[p.ID] = p
	}

	platforms := make([]resolvedPlatform, 0, len(targetPlatforms))
	for _, tp := range targetPlatforms {
		p, ok := byID[tp.ID]
		if !ok {
			continue
		}

		// Resolve post type slugs for this platform entry.
		// If the campaign specifies post_types, use only those slugs and enforce
		// them in validateOutput. Otherwise fall back to all platform slugs with
		// no enforcement.
		var slugs []string
		var allowedSlugs []string
		if len(tp.PostTypes) > 0 {
			// Use only the slugs selected for this campaign. Unknown slugs are
			// passed through as-is so nothing is silently lost.
			slugs = append(slugs, tp.PostTypes...)
			allowedSlugs = slugs
		} else {
			for slug := range p.PostTypes {
				slugs = append(slugs, slug)
			}
			// allowedSlugs stays nil → no contentType enforcement
		}

		platforms = append(platforms, resolvedPlatform{
			ID:           p.ID,
			Name:         p.Name,
			PostTypes:    strings.Join(slugs, ", "),
			AllowedSlugs: allowedSlugs,
			Cadence:      p.Cadence,
			Constraints:  p.Constraints,
		})
	}
	return platforms, nil
}

// resolvePhaseByID returns the campaign phase with the given id (CON-114).
func resolvePhaseByID(campaign *models.Campaign, phaseID string) (resolvedPhase, bool) {
	if campaign.CampaignType == nil {
		return resolvedPhase{}, false
	}
	for _, p := range campaign.CampaignType.Phases {
		if p.ID == phaseID {
			return resolvedPhase{ID: p.ID, Name: p.Name, Purpose: p.Purpose, Sequence: p.Sequence}, true
		}
	}
	return resolvedPhase{}, false
}

// selectPlatforms restricts the resolved platforms to the requested ids — each
// must be one of the campaign's target platforms (CON-114 "stay in scope") —
// and, when postType is given, narrows every selected platform to that single
// post type, which the platform must support.
func selectPlatforms(all []resolvedPlatform, ids []string, postType string) ([]resolvedPlatform, error) {
	if len(ids) == 0 {
		return nil, &ValidationError{Msg: "no platforms specified"}
	}
	byID := make(map[string]resolvedPlatform, len(all))
	for _, p := range all {
		byID[p.ID] = p
	}
	out := make([]resolvedPlatform, 0, len(ids))
	for _, id := range ids {
		p, ok := byID[id]
		if !ok {
			return nil, &ValidationError{Msg: "platform " + id + " is not a target platform of this campaign"}
		}
		if postType != "" {
			if !platformSupportsSlug(p, postType) {
				return nil, &ValidationError{Msg: "platform " + p.Name + " does not support post type " + postType}
			}
			p.PostTypes = postType
			p.AllowedSlugs = []string{postType}
		}
		out = append(out, p)
	}
	return out, nil
}

// platformSupportsSlug reports whether slug is among the platform's
// (comma-separated) available post-type slugs.
func platformSupportsSlug(p resolvedPlatform, slug string) bool {
	for s := range strings.SplitSeq(p.PostTypes, ",") {
		if strings.TrimSpace(s) == slug {
			return true
		}
	}
	return false
}
