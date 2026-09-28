package content_plan

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func validateInput(ctx context.Context, campaignID string, campaignRepo repository.CampaignRepository) (*models.Campaign, error) {
	c, err := campaignRepo.GetByID(ctx, campaignID)
	if err != nil {
		return nil, err
	}

	var missing []string
	if c.Name == "" {
		missing = append(missing, "name")
	}
	if c.Description == "" {
		missing = append(missing, "description")
	}
	if c.TargetPersona == "" {
		missing = append(missing, "target_persona")
	}
	if c.KeyMessages == "" {
		missing = append(missing, "key_messages")
	}
	if c.ToneGuidelines == "" {
		missing = append(missing, "tone_guidelines")
	}
	if c.CampaignTypeID == "" {
		missing = append(missing, "campaign_type_id")
	}
	if c.CampaignType != nil && len(c.CampaignType.Phases) == 0 {
		return nil, &ValidationError{Msg: "campaign type has no phases — add at least one phase before generating content"}
	}
	if c.StartDate == nil {
		missing = append(missing, "start_date")
	}
	if c.EndDate == nil {
		missing = append(missing, "end_date")
	}
	if len(c.TargetPlatforms) == 0 {
		missing = append(missing, "target_platforms")
	}
	if len(missing) > 0 {
		return nil, &ValidationError{Msg: "missing required campaign fields: " + strings.Join(missing, ", ")}
	}

	if !c.StartDate.Before(*c.EndDate) {
		return nil, &ValidationError{Msg: "start_date must be before end_date"}
	}
	if c.EndDate.Sub(*c.StartDate) < 24*time.Hour {
		return nil, &ValidationError{Msg: "date range must be at least 1 day"}
	}

	// NB: we deliberately do NOT hard-check that every id in c.AssetIDs still
	// exists. An asset can be deleted while its id lingers in a campaign's
	// asset_ids, and resolveAssets → collectReadyCandidateIDs already
	// skips missing/unready ids with a warning ("asset %q could not be loaded —
	// skipped") and proceeds without asset context when all are gone. A strict
	// check here only turned that recoverable stale reference into a fatal tool
	// error that 502'd the whole assistant turn.

	return c, nil
}

// postValidator is the per-post check used by both the legacy
// validateOutput pass and the per-post inline-persist path. Built once per
// run by buildPostValidator and applied to each parsed post; returns nil
// when the post passes, an error describing the rejection otherwise.
type postValidator func(post DraftPost) error

// buildPostValidator validates against the campaign's full phase set and
// [start, end] window — the full-plan path. Delegates to newPostValidator.
func buildPostValidator(campaign *models.Campaign, platforms []resolvedPlatform) postValidator {
	validPhaseIDs := make(map[string]bool, len(campaign.CampaignType.Phases))
	for _, ph := range campaign.CampaignType.Phases {
		validPhaseIDs[ph.ID] = true
	}
	return newPostValidator(platforms, validPhaseIDs,
		campaign.StartDate.Format("2006-01-02"), campaign.EndDate.Format("2006-01-02"))
}

// newPostValidator captures the per-run constraint sets — platform allowlist,
// per-platform contentType allowlist, phase allowlist, and the [startDate,
// endDate] publish window — into a closure callable inline from
// generatePostsStreaming. Parameterizing the phases + window (rather than
// reading them off the campaign) lets the CON-114 targeted path restrict to a
// single phase and a custom window while reusing the same checks. The returned
// function is goroutine-safe (read-only over the captured maps).
func newPostValidator(platforms []resolvedPlatform, validPhaseIDs map[string]bool, startDate, endDate string) postValidator {
	validPlatformIDs := make(map[string]bool, len(platforms))
	platformAllowedSlugs := make(map[string]map[string]bool)
	for _, p := range platforms {
		validPlatformIDs[p.ID] = true
		if len(p.AllowedSlugs) > 0 {
			m := make(map[string]bool, len(p.AllowedSlugs))
			for _, s := range p.AllowedSlugs {
				m[s] = true
			}
			platformAllowedSlugs[p.ID] = m
		}
	}

	return func(post DraftPost) error {
		if !validPlatformIDs[post.PlatformID] {
			return fmt.Errorf("unknown platformId %q", post.PlatformID)
		}
		if allowed, ok := platformAllowedSlugs[post.PlatformID]; ok {
			if !allowed[post.ContentType] {
				return fmt.Errorf("contentType %q not allowed for platform %q", post.ContentType, post.PlatformID)
			}
		}
		d := post.PublishDate
		if d < startDate || d > endDate {
			return fmt.Errorf("publishDate %q is outside campaign range", d)
		}
		if !validPhaseIDs[post.PhaseID] {
			return fmt.Errorf("unknown phaseId %q", post.PhaseID)
		}
		return nil
	}
}

// validateOutput is retained as a thin wrapper for callers that already
// have the full post slice in hand (e.g. tests). The streaming path
// applies the same validator inline rather than gathering the slice
// first.
func validateOutput(posts []DraftPost, campaign *models.Campaign, platforms []resolvedPlatform) ([]DraftPost, []string) {
	validate := buildPostValidator(campaign, platforms)
	valid := make([]DraftPost, 0, len(posts))
	var warnings []string
	for _, post := range posts {
		if err := validate(post); err != nil {
			warnings = append(warnings, fmt.Sprintf("post %q dropped: %s", post.Title, err))
			continue
		}
		valid = append(valid, post)
	}
	return valid, warnings
}
