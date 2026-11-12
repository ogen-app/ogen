package packs

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_check"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
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

// BrandFinding is a way a post breaks the brand, with its fixes: rewriting
// the post, while its copy isn't locked, and opening it.
type BrandFinding struct {
	brand_check.Finding
	// Key dismisses the finding, in the post's campaign, while the post
	// stays the same.
	Key string `json:"key"`
	// CampaignID is the post's campaign, which opening it and dismissing
	// the finding need.
	CampaignID string     `json:"campaignId"`
	Title      string     `json:"title,omitempty"`
	Fixes      []FixOffer `json:"fixes"`
}

// BrandRef names a voice the posts were checked against.
type BrandRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CheckedAgainst is the brand the posts were read against: the voices they
// are written in and whether there were guardrails.
type CheckedAgainst struct {
	Voices     []BrandRef `json:"voices"`
	Guardrails bool       `json:"guardrails"`
}

// CheckBrandOutput is what the check found. Clean counts the posts checked
// with no finding left; Left the posts past the cap that weren't read.
type CheckBrandOutput struct {
	Checked        int            `json:"checked"`
	Clean          int            `json:"clean"`
	Left           int            `json:"left,omitzero"`
	CheckedAgainst CheckedAgainst `json:"checkedAgainst"`
	Findings       []BrandFinding `json:"findings"`
	// Dismissed counts findings left out because the user dismissed them.
	Dismissed int `json:"dismissed,omitzero"`
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
	out := &CheckBrandOutput{Left: left, Findings: []BrandFinding{}, CheckedAgainst: CheckedAgainst{Voices: []BrandRef{}}}
	if len(posts) == 0 {
		return out, nil
	}
	// Posts written for the same brand block are checked together.
	ctx = brandresolve.WithMemo(ctx)
	groups := map[string]*brand_check.Group{}
	var order []string
	byID := map[string]*models.Post{}
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
		if v := r.Voice; v != nil && !slices.ContainsFunc(out.CheckedAgainst.Voices, func(b BrandRef) bool { return b.ID == v.ID }) {
			out.CheckedAgainst.Voices = append(out.CheckedAgainst.Voices, BrandRef{ID: v.ID, Name: v.Name})
		}
		out.CheckedAgainst.Guardrails = out.CheckedAgainst.Guardrails || r.Guardrails != nil
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
		byID[p.ID] = &p
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
	if err := out.addFindings(ctx, d, res.Findings, byID); err != nil {
		return nil, err
	}
	slices.SortStableFunc(out.Findings, func(a, b BrandFinding) int { return cmp.Compare(severityRank(a.Severity), severityRank(b.Severity)) })
	return out, nil
}

// addFindings keys the check's findings and offers their fixes, leaving out
// those dismissed in the post's campaign, and counts the posts left clean.
func (out *CheckBrandOutput) addFindings(ctx context.Context, d Deps, found []brand_check.Finding, byID map[string]*models.Post) error {
	dismissed := map[string]map[string]bool{}
	flagged := map[string]bool{}
	for _, f := range found {
		p, ok := byID[f.PostID]
		if !ok {
			continue
		}
		if _, ok := dismissed[p.CampaignID]; !ok {
			keys, err := d.dismissedKeys(ctx, p.CampaignID)
			if err != nil {
				return err
			}
			dismissed[p.CampaignID] = keys
		}
		bf := BrandFinding{Finding: f, Key: brandFindingKey(p, f), CampaignID: p.CampaignID, Title: p.Title}
		if dismissed[p.CampaignID][bf.Key] {
			out.Dismissed++
			continue
		}
		fixes, err := brandFixes(ctx, d, p, f)
		if err != nil {
			return err
		}
		bf.Fixes = fixes
		flagged[p.ID] = true
		out.Findings = append(out.Findings, bf)
	}
	out.Clean = max(out.Checked-len(flagged), 0)
	return nil
}

// brandFindingKey keys a finding by its post's text, the rule it breaks and
// the words it quotes, so it holds while the post stays the same however the
// check words the issue; the quote, case and spacing aside, tells two breaks
// of one rule apart.
func brandFindingKey(p *models.Post, f brand_check.Finding) string {
	quote := strings.Join(strings.Fields(strings.ToLower(f.Quote)), " ")
	h := sha256.New()
	for _, s := range []string{p.CampaignID, p.ID, f.Rule, quote, p.Title + "\x00" + p.Content} {
		h.Write([]byte(s + "|"))
	}
	return "brand_" + f.Rule + ":" + hex.EncodeToString(h.Sum(nil))[:12]
}

// brandFixes offers to rewrite the post to fix the finding, while its copy
// isn't locked and the writer is here, and to open it.
func brandFixes(ctx context.Context, d Deps, p *models.Post, f brand_check.Finding) ([]FixOffer, error) {
	var out []FixOffer
	if d.Revise != nil && !p.Status.IsSubmitted() {
		instruction := "Bring the post in line with the brand. The issue: " + f.Issue
		if f.Suggestion != "" {
			instruction += " Fix it this way: " + f.Suggestion
		}
		id, err := assistantkit.Offer(ctx, assistantkit.Proposal{Tool: "revisePost",
			Args: RevisePostInput{CampaignID: p.CampaignID, PostID: p.ID, Instruction: instruction}, Summary: "Rewrite it to follow the brand"})
		if err != nil {
			return nil, err
		}
		out = append(out, FixOffer{Fix: calendarcheck.Fix{Kind: calendarcheck.FixKind(consistency.FixRevisePost), Title: "Rewrite it to follow the brand", PostID: p.ID}, ProposalID: id})
	}
	return append(out, FixOffer{Fix: calendarcheck.Fix{Kind: calendarcheck.FixOpenPost, Title: "Open the post", PostID: p.ID}}), nil
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
