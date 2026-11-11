package compose

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Draft errors, all a 400.
var (
	ErrNoPlatform     = errors.New("pick the platform to write for: none of the ideas suggests one")
	ErrTooManyDrafts  = errors.New("that is more posts than one request writes")
	ErrPostTypeOneFor = errors.New("a post type applies to one platform; name a single platform")
)

// DraftRequest is one call to the writer: count posts on one platform from
// the source material. It mirrors the draft_post flow's request, so the
// server adapts one to the other without this package knowing the flow.
type DraftRequest struct {
	CampaignID     string
	PlatformID     string
	PostType       string
	Count          int
	SourceMaterial string
	Instruction    string
	// WindowStart and WindowEnd bound the publish dates (YYYY-MM-DD); with
	// neither, the drafts are undated.
	WindowStart  string
	WindowEnd    string
	PhaseID      string
	UsedAssetIDs []string
	// CreatedBy is who the posts and their source notes are attributed to.
	CreatedBy string
}

// Written is a post the writer created. PublishDate (YYYY-MM-DD) is empty
// for an unscheduled draft.
type Written struct {
	PostID      string `json:"postId"`
	Title       string `json:"title"`
	PlatformID  string `json:"platformId"`
	PublishDate string `json:"publishDate,omitempty"`
}

// Drafter writes finished posts and persists them as drafts. Warnings are
// drafts it dropped; an error stops the run.
type Drafter func(ctx context.Context, req DraftRequest) ([]Written, []string, error)

// DraftInput is one request to write posts from ideas. Each idea becomes its
// own post on each platform, or with Together one post per platform from all
// of them. Fields left empty default as for Compile; the platforms to the
// first idea plan that suggests one.
type DraftInput struct {
	IdeaIDs     []string
	CampaignID  string
	PlatformIDs []string
	PostType    string
	PhaseID     string
	TargetDate  *models.CalendarDate
	Instruction string
	Together    bool
	Actor       string
}

// DraftPlan is a checked request: the jobs to run, each one writer call.
type DraftPlan struct {
	Ideas []models.Idea
	Jobs  []DraftJob
}

// DraftJob is one post to write and the ideas it comes from.
type DraftJob struct {
	Request DraftRequest
	IdeaIDs []string
}

// DraftedPost is a written post, the ideas it came from and the assets the
// writer was given, its sources.
type DraftedPost struct {
	Written
	IdeaIDs      []string `json:"ideaIds"`
	UsedAssetIDs []string `json:"usedAssetIds"`
}

// DraftResult is every post written, in job order, and what was dropped.
type DraftResult struct {
	Posts    []DraftedPost `json:"posts"`
	Warnings []string      `json:"warnings,omitempty"`
}

// PlanDraft checks a draft request without writing anything: the ideas, the
// campaign, the platforms (a named one must be the campaign's), a named phase
// (one of the campaign type's), and at most maxPosts posts.
func (s *Service) PlanDraft(ctx context.Context, in DraftInput, maxPosts int) (*DraftPlan, error) {
	ideas, err := s.loadIdeas(ctx, in.IdeaIDs)
	if err != nil {
		return nil, err
	}
	campaign, err := s.campaign(ctx, in.CampaignID, ideas)
	if err != nil {
		return nil, err
	}
	// One more platform than the campaign has is enough to find one that
	// isn't its own.
	platforms := dedupe(in.PlatformIDs, len(campaign.TargetPlatforms))
	if err := checkAsked(campaign, platforms, in.PostType, in.PhaseID); err != nil {
		return nil, err
	}
	plan := firstPlan(ideas, campaign.ID)
	postType := in.PostType
	if len(platforms) == 0 {
		// The suggested platform and its post type, if they still fit.
		suggested, suggestedType, _ := fitTarget(campaign, deref(plan.PlatformID), deref(plan.PostType), false, false)
		if suggested == "" {
			return nil, ErrNoPlatform
		}
		platforms, postType = []string{suggested}, suggestedType
	}

	groups := [][]models.Idea{ideas}
	if !in.Together {
		groups = make([][]models.Idea, len(ideas))
		for i := range ideas {
			groups[i] = ideas[i : i+1]
		}
	}
	if len(groups)*len(platforms) > maxPosts {
		return nil, fmt.Errorf("%w: %d posts asked for, at most %d", ErrTooManyDrafts, len(groups)*len(platforms), maxPosts)
	}

	date := in.TargetDate
	if date == nil {
		date = plan.TargetDate
	}
	var window string
	if date != nil {
		window = date.String()
	}
	phase := cmp.Or(in.PhaseID, deref(plan.PhaseID))
	out := &DraftPlan{Ideas: ideas}
	for _, group := range groups {
		ids := make([]string, len(group))
		for i, idea := range group {
			ids[i] = idea.ID
		}
		for _, p := range platforms {
			out.Jobs = append(out.Jobs, DraftJob{IdeaIDs: ids, Request: DraftRequest{
				CampaignID:     campaign.ID,
				PlatformID:     p,
				PostType:       postType,
				Count:          1,
				SourceMaterial: sourceMaterial(group),
				Instruction:    strings.TrimSpace(in.Instruction),
				WindowStart:    window,
				WindowEnd:      window,
				PhaseID:        phase,
				UsedAssetIDs:   sourceAssets(group),
				CreatedBy:      in.Actor,
			}})
		}
	}
	return out, nil
}

// RunDraft runs a plan's jobs in order and links every written post to its
// ideas. onPost, when set, sees each post as it lands. A writer error stops
// the run; the posts written before it are kept, linked and returned.
func (s *Service) RunDraft(ctx context.Context, plan *DraftPlan, onPost func(DraftedPost)) (*DraftResult, error) {
	res := &DraftResult{Posts: []DraftedPost{}}
	for _, job := range plan.Jobs {
		written, warnings, err := s.draft(ctx, withWindow(job.Request))
		res.Warnings = append(res.Warnings, warnings...)
		for _, w := range written {
			if err := s.links.Link(ctx, s.db, w.PostID, job.IdeaIDs); err != nil {
				return res, fmt.Errorf("link post %s to its ideas: %w", w.PostID, err)
			}
			p := DraftedPost{Written: w, IdeaIDs: job.IdeaIDs, UsedAssetIDs: slices.Clone(job.Request.UsedAssetIDs)}
			if p.UsedAssetIDs == nil {
				p.UsedAssetIDs = []string{}
			}
			res.Posts = append(res.Posts, p)
			if onPost != nil {
				onPost(p)
			}
		}
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// Write runs one writer call outside a plan — posts written from source
// material other than ideas, such as posts that performed well — and links
// each written post to ideaIDs, its provenance.
func (s *Service) Write(ctx context.Context, req DraftRequest, ideaIDs []string, onPost func(DraftedPost)) (*DraftResult, error) {
	if s.draft == nil {
		return nil, errors.New("compose: no writer")
	}
	return s.RunDraft(ctx, &DraftPlan{Jobs: []DraftJob{{Request: req, IdeaIDs: ideaIDs}}}, onPost)
}

// withWindow completes a one-sided publish window, which the writer needs
// whole: a lone start or end is the single day. No window at all leaves the
// drafts undated, off the calendar until someone dates them.
func withWindow(req DraftRequest) DraftRequest {
	req.WindowStart = cmp.Or(req.WindowStart, req.WindowEnd)
	req.WindowEnd = cmp.Or(req.WindowEnd, req.WindowStart)
	return req
}

// WithDrafter returns s writing through d.
func (s *Service) WithDrafter(d Drafter) *Service {
	s.draft = d
	return s
}

// checkAsked checks the platforms, post type and phase a request names
// against the campaign.
func checkAsked(campaign *models.Campaign, platforms []string, postType, phaseID string) error {
	if postType != "" && len(platforms) != 1 {
		return ErrPostTypeOneFor
	}
	for _, p := range platforms {
		if _, _, err := fitTarget(campaign, p, postType, true, postType != ""); err != nil {
			return err
		}
	}
	if phaseID != "" && !hasPhase(campaign, phaseID) {
		return ErrInvalidPhase
	}
	return nil
}

// hasPhase reports whether phaseID is a phase of the campaign's type.
func hasPhase(campaign *models.Campaign, phaseID string) bool {
	return campaign.CampaignType != nil && slices.ContainsFunc(campaign.CampaignType.Phases,
		func(p models.CampaignTypePhase) bool { return p.ID == phaseID })
}

// firstPlan is the first idea plan that sets each field; a phase only from
// an idea of campaignID.
func firstPlan(ideas []models.Idea, campaignID string) models.IdeaPlan {
	plan := models.IdeaPlan{}
	for _, idea := range slices.Backward(ideas) {
		p := idea.Plan
		// A post type belongs to the platform it was suggested with.
		if p.PlatformID != nil {
			plan.PlatformID, plan.PostType = p.PlatformID, p.PostType
		} else if plan.PlatformID == nil {
			plan.PostType = cmp.Or(p.PostType, plan.PostType)
		}
		plan.TargetDate = cmp.Or(p.TargetDate, plan.TargetDate)
		if idea.CampaignID != nil && *idea.CampaignID == campaignID {
			plan.PhaseID = cmp.Or(p.PhaseID, plan.PhaseID)
		}
	}
	return plan
}

// sourceMaterial is what the writer writes from: each idea's title, angle,
// reasoning and sources, or its note.
func sourceMaterial(ideas []models.Idea) string {
	var b strings.Builder
	for i, idea := range ideas {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "Idea: %s", strings.TrimSpace(idea.Title))
		if idea.Angle == nil && idea.Rationale == nil {
			if note := strings.TrimSpace(idea.Note); note != "" {
				fmt.Fprintf(&b, "\n%s", note)
			}
			continue
		}
		if a := strings.TrimSpace(deref(idea.Angle)); a != "" {
			fmt.Fprintf(&b, "\nAngle: %s", a)
		}
		if r := strings.TrimSpace(deref(idea.Rationale)); r != "" {
			fmt.Fprintf(&b, "\nWhy it works: %s", r)
		}
		if len(idea.Sources) > 0 {
			fmt.Fprintf(&b, "\nFrom the source: %s", strings.Join(idea.Sources, " · "))
		}
	}
	return b.String()
}
