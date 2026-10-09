package assetanalysis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/asset_analysis"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
)

// Caps on the hint lists a lens carries, so a busy campaign cannot crowd out
// the source.
const (
	maxSiblings       = 20
	maxSiblingRunes   = 2000
	maxExistingIdeas  = 100
	maxPostTitles     = 50
	maxPostTitleRunes = 200
)

// buildLens gathers the context an analysis is read in. campaign is nil for
// the workspace lens. lensID is the analysis being produced; its own inbox
// ideas are about to be replaced, so they are not listed as already captured.
func (s *Service) buildLens(ctx context.Context, tenantID string, asset *models.Asset, campaign *models.Campaign, lensID string) (asset_analysis.Lens, error) {
	lens := asset_analysis.Lens{BrandBlock: s.brandBlock(ctx, campaign)}
	if tenant, err := s.repos.Tenants.GetByID(ctx, tenantID); err == nil {
		lens.WorkspaceName = tenant.Name
	}
	filter := repository.IdeaListFilter{Unfiled: true}
	if campaign != nil {
		lens.Campaign = brief(campaign)
		filter = repository.IdeaListFilter{CampaignID: campaign.ID}
		var err error
		if lens.Siblings, err = s.siblings(ctx, asset.ID, campaign); err != nil {
			return lens, err
		}
		if lens.PostTitles, err = s.postTitles(ctx, campaign.ID); err != nil {
			return lens, err
		}
	}
	ideas, err := s.repos.Ideas.List(ctx, filter)
	if err != nil {
		return lens, fmt.Errorf("list ideas: %w", err)
	}
	for _, idea := range ideas {
		if idea.Verdict == nil && idea.AssetAnalysisID != nil && *idea.AssetAnalysisID == lensID {
			continue
		}
		lens.ExistingIdeas = append(lens.ExistingIdeas, idea.Title)
	}
	if len(lens.ExistingIdeas) > maxExistingIdeas {
		lens.ExistingIdeas = lens.ExistingIdeas[len(lens.ExistingIdeas)-maxExistingIdeas:]
	}
	return lens, nil
}

// brandBlock renders the brand voice, audience, guardrails and facts. It
// fails open: without brand material the analysis still runs.
func (s *Service) brandBlock(ctx context.Context, campaign *models.Campaign) string {
	resolved, err := brandresolve.Resolve(ctx, s.repos.Brands, campaign, nil)
	if err != nil {
		slog.WarnContext(ctx, "brand resolution failed; analysing without it", logging.AttrComponent, logComponent, logging.AttrError, err)
	}
	return resolved.PromptBlock("")
}

func brief(c *models.Campaign) *asset_analysis.CampaignBrief {
	b := &asset_analysis.CampaignBrief{
		Name:           c.Name,
		Description:    c.Description,
		TargetPersona:  c.TargetPersona,
		KeyMessages:    c.KeyMessages,
		ToneGuidelines: c.ToneGuidelines,
		Language:       c.Language,
	}
	if t := c.CampaignType; t != nil {
		b.CampaignType = t.Name
		for _, p := range t.Phases {
			b.Phases = append(b.Phases, p.Name)
		}
	}
	if c.EstimatedPostCount != nil && *c.EstimatedPostCount > 0 {
		b.Goal = fmt.Sprintf("%d posts per %s", *c.EstimatedPostCount, c.GoalCadence)
	}
	return b
}

// siblings are the workspace summaries of the campaign's other ready assets.
func (s *Service) siblings(ctx context.Context, assetID string, campaign *models.Campaign) ([]asset_analysis.SiblingSummary, error) {
	ids := slices.DeleteFunc(slices.Clone(campaign.AssetIDs), func(id string) bool { return id == assetID })
	if len(ids) > maxSiblings {
		ids = ids[:maxSiblings]
	}
	if len(ids) == 0 {
		return nil, nil
	}
	assets, err := s.repos.Assets.ListMeta(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list sibling assets: %w", err)
	}
	var out []asset_analysis.SiblingSummary
	for _, a := range assets {
		lens, err := s.repos.Analyses.GetLens(ctx, a.ID, nil)
		if err != nil || lens.Status != models.AssetAnalysisStatusReady || lens.Summary == "" {
			continue
		}
		out = append(out, asset_analysis.SiblingSummary{Title: a.Title, Summary: clamp(lens.Summary, maxSiblingRunes)})
	}
	return out, nil
}

func (s *Service) postTitles(ctx context.Context, campaignID string) ([]string, error) {
	posts, err := s.repos.Posts.ListRowsByCampaign(ctx, campaignID, "title")
	if err != nil {
		return nil, fmt.Errorf("list campaign posts: %w", err)
	}
	var out []string
	for _, p := range posts {
		if p.Title != "" {
			out = append(out, clamp(p.Title, maxPostTitleRunes))
		}
		if len(out) == maxPostTitles {
			break
		}
	}
	return out, nil
}

// fingerprint identifies what an analysis was made from: the source text and
// the lens's brand and brief. The hint lists (sibling summaries, ideas and
// posts already captured) are left out on purpose: they shift whenever any
// other asset is analysed, and re-reading an unchanged source against an
// unchanged brief only to dodge a few more duplicates is not worth a run.
func fingerprint(src asset_analysis.Source, partial bool, lens asset_analysis.Lens) string {
	payload, _ := json.Marshal(struct {
		Source   asset_analysis.Source
		Partial  bool
		Brand    string
		Campaign *asset_analysis.CampaignBrief
	}{src, partial, lens.BrandBlock, lens.Campaign})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func clamp(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
