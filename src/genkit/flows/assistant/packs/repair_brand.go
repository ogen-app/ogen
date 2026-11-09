package packs

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_check"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/calendarcheck"
)

// maxBrandCheck caps the posts one brand check reads.
const maxBrandCheck = 25

// CheckBrandInput is the posts to check: the ones named, else a campaign's.
type CheckBrandInput struct {
	PostIDs    []string `json:"postIds,omitempty"    jsonschema:"description=The posts to check, e.g. the selected ones; the thread's post or campaign when omitted"`
	CampaignID string   `json:"campaignId,omitempty" jsonschema:"description=Check this campaign's unpublished posts"`
}

// BrandFinding is a way a post breaks the brand, with its fix.
type BrandFinding struct {
	brand_check.Finding
	// CampaignID is the post's campaign, which opening it needs.
	CampaignID string   `json:"campaignId"`
	Title      string   `json:"title,omitempty"`
	Fix        FixOffer `json:"fix"`
}

// CheckBrandOutput is what the check found. Left counts the posts past the
// cap that weren't read.
type CheckBrandOutput struct {
	Checked  int            `json:"checked"`
	Left     int            `json:"left,omitzero"`
	Findings []BrandFinding `json:"findings"`
}

// postsToCheck are the posts named, else the thread's post, else the
// campaign's unpublished ones; capped, with how many were left.
func postsToCheck(ctx context.Context, d Deps, in CheckBrandInput) ([]models.Post, int, error) {
	ids := slices.Compact(slices.Sorted(slices.Values(trimAll(in.PostIDs))))
	if s, ok := assistant.Scope(ctx); len(ids) == 0 && in.CampaignID == "" && ok && s.Kind == models.AssistantScopePost {
		ids = []string{s.ID}
	}
	var posts []models.Post
	if len(ids) > 0 {
		for _, id := range ids {
			p, err := d.Posts.GetByID(ctx, id)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, 0, assistantkit.Refused("post " + id + " doesn't exist")
			}
			if err != nil {
				return nil, 0, err
			}
			posts = append(posts, *p)
		}
	} else {
		c, err := d.loadCampaign(ctx, CampaignRef{CampaignID: in.CampaignID})
		if err != nil {
			return nil, 0, err
		}
		all, err := d.Posts.ListByCampaign(ctx, c.ID)
		if err != nil {
			return nil, 0, err
		}
		for _, p := range all {
			if p.Status != models.PostStatusPublished && strings.TrimSpace(p.Content) != "" {
				posts = append(posts, p)
			}
		}
	}
	left := max(len(posts)-maxBrandCheck, 0)
	return posts[:len(posts)-left], left, nil
}

func checkBrand(ctx context.Context, d Deps, in CheckBrandInput) (*CheckBrandOutput, error) {
	posts, left, err := postsToCheck(ctx, d, in)
	if err != nil {
		return nil, err
	}
	out := &CheckBrandOutput{Left: left, Findings: []BrandFinding{}}
	if len(posts) == 0 {
		return out, nil
	}
	// Posts written for the same brand block are checked together.
	ctx = brandresolve.WithMemo(ctx)
	groups := map[string]*brand_check.Group{}
	var order []string
	titles, campaignIDs := map[string]string{}, map[string]string{}
	campaigns := map[string]*models.Campaign{}
	for _, p := range posts {
		c, ok := campaigns[p.CampaignID]
		if !ok {
			if c, err = d.Campaigns.GetByID(ctx, p.CampaignID); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			campaigns[p.CampaignID] = c
		}
		r, err := brandresolve.Resolve(ctx, d.Brand, c, &p)
		if err != nil {
			return nil, err
		}
		block := r.PromptBlock(p.PlatformID)
		g, ok := groups[block]
		if !ok {
			g = &brand_check.Group{Brand: block}
			if r.Guardrails != nil {
				g.BannedWords = r.Guardrails.BannedWords
			}
			groups[block] = g
			order = append(order, block)
		}
		platform := p.PlatformID
		if p.Platform != nil {
			platform = cmp.Or(p.Platform.Name, platform)
		}
		g.Posts = append(g.Posts, brand_check.Post{ID: p.ID, Platform: platform, Text: p.Content})
		titles[p.ID], campaignIDs[p.ID] = p.Title, p.CampaignID
	}
	req := brand_check.Request{}
	for _, b := range order {
		req.Groups = append(req.Groups, *groups[b])
	}
	res, err := d.CheckBrand(ctx, req)
	if err != nil {
		return nil, assistantkit.Refused("couldn't check the posts: " + err.Error())
	}
	out.Checked = res.Checked
	for _, f := range res.Findings {
		out.Findings = append(out.Findings, BrandFinding{Finding: f, CampaignID: campaignIDs[f.PostID], Title: titles[f.PostID],
			Fix: FixOffer{Fix: calendarcheck.Fix{Kind: calendarcheck.FixOpenPost, Title: "Open the post", PostID: f.PostID}}})
	}
	slices.SortStableFunc(out.Findings, func(a, b BrandFinding) int { return cmp.Compare(severityRank(a.Severity), severityRank(b.Severity)) })
	return out, nil
}

func severityRank(s string) int {
	switch s {
	case "high":
		return 0
	case "medium":
		return 1
	}
	return 2
}
