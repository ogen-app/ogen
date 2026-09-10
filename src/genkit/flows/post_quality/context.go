package post_quality

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/domain/models"
)

// maxSummaryChars caps the combined asset previews at ~1000 tokens.
const maxSummaryChars = 4000

// previewChars is the target length for a single asset preview.
const previewChars = 200

// renderedPrompts holds the two prompt strings ready for the model call.
// system is the static rubric (stable, cacheable prefix); user carries
// the per-post campaign context, platform conventions, and body.
type renderedPrompts struct {
	system        string
	user          string
	captionScoped bool
}

// buildContext assembles the per-call template data from the (hydrated)
// Post and renders both prompt blocks. The Post is expected to come from
// PostRepository.GetByID, which hydrates Campaign, Platform,
// CampaignTypePhase, and UsedAssets. Its Content has already been resolved
// by the caller to the latest committed version when one exists (CON-184),
// so PostBody below is the assessed snapshot rather than the live HEAD.
func buildContext(
	ctx context.Context,
	post *models.Post,
	cfg PostQualityFlowConfig,
	repos PostQualityRepos,
) (*renderedPrompts, error) {
	// Campaign (the brief). Prefer the hydrated relation; fall back to a
	// direct load so a non-hydrated Post still works.
	campaign := post.Campaign
	if campaign == nil {
		c, err := repos.Campaigns.GetByID(ctx, post.CampaignID)
		if err != nil {
			return nil, fmt.Errorf("load campaign: %w", err)
		}
		campaign = c
	}

	var phaseName, phaseDescription string
	if post.CampaignTypePhase != nil {
		phaseName = post.CampaignTypePhase.Name
		phaseDescription = post.CampaignTypePhase.Purpose
	}

	platformName, postTypeLabel, conventions := platformContext(post)

	assets, err := buildAssetContexts(ctx, post.UsedAssetIDs, repos)
	if err != nil {
		return nil, err
	}

	captionScoped := len(post.MediaURLs) > 0

	data := assessmentTemplateData{
		Platform:            platformName,
		PostType:            postTypeLabel,
		CaptionScoped:       captionScoped,
		PlatformConventions: conventions,
		CampaignName:        campaign.Name,
		CampaignDescription: campaign.Description,
		TargetPersona:       campaign.TargetPersona,
		KeyMessages:         campaign.KeyMessages,
		ToneGuidelines:      campaign.ToneGuidelines,
		Language:            campaign.Language,
		PhaseName:           phaseName,
		PhaseDescription:    phaseDescription,
		Assets:              assets,
		PostTitle:           post.Title,
		PostBody:            post.Content,
		SuggestionCap:       cfg.SuggestionCap,
	}

	system, err := cfg.tmpl.renderSystem()
	if err != nil {
		return nil, err
	}
	user, err := cfg.tmpl.renderUser(data)
	if err != nil {
		return nil, err
	}
	return &renderedPrompts{system: system, user: user, captionScoped: captionScoped}, nil
}

// platformContext derives the platform display name, the post-type label,
// and a one-line conventions string from the hydrated Platform. It
// degrades gracefully when the Platform relation is missing.
func platformContext(post *models.Post) (name, postTypeLabel, conventions string) {
	postTypeLabel = post.PlatformPostType
	if post.Platform == nil {
		return post.PlatformID, postTypeLabel, ""
	}
	name = post.Platform.Name
	if label, ok := post.Platform.PostTypes[post.PlatformPostType]; ok && label != "" {
		postTypeLabel = label
	}
	var parts []string
	if post.Platform.Cadence != "" {
		parts = append(parts, "cadence: "+post.Platform.Cadence)
	}
	if post.Platform.Constraints != "" {
		parts = append(parts, post.Platform.Constraints)
	}
	return name, postTypeLabel, strings.Join(parts, "; ")
}

// buildAssetContexts loads each used asset's title and a short preview of
// its first chunk, in parallel, and proportionally trims previews to a
// combined character budget. A failed asset load is skipped rather than
// failing the whole evaluation.
func buildAssetContexts(ctx context.Context, assetIDs []string, repos PostQualityRepos) ([]assetContext, error) {
	if len(assetIDs) == 0 {
		return nil, nil
	}

	type result struct {
		summary assetContext
		ok      bool
	}
	results := make([]result, len(assetIDs))
	var wg sync.WaitGroup
	for i, id := range assetIDs {
		wg.Go(func() {
			idx, assetID := i, id
			asset, err := repos.Assets.GetByID(ctx, assetID)
			if err != nil {
				return
			}
			preview := truncateRunes(asset.Content, previewChars)
			if chunks, err := repos.Chunks.GetByAssetID(ctx, assetID); err == nil && len(chunks) > 0 {
				preview = truncateRunes(chunks[0].Content, previewChars)
			}
			results[idx] = result{summary: assetContext{Title: asset.Title, Preview: preview}, ok: true}
		})
	}
	wg.Wait()

	summaries := make([]assetContext, 0, len(assetIDs))
	for _, r := range results {
		if r.ok {
			summaries = append(summaries, r.summary)
		}
	}

	totalChars := 0
	for _, s := range summaries {
		totalChars += len(s.Preview)
	}
	if totalChars > maxSummaryChars && len(summaries) > 0 {
		budget := maxSummaryChars / len(summaries)
		for i := range summaries {
			summaries[i].Preview = truncateRunes(summaries[i].Preview, budget)
		}
	}
	return summaries, nil
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
