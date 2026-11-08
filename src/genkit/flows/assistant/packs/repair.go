package packs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/campaignphase"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/calendarcheck"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/reschedule"
	"github.com/ogen-app/ogen/src/usecase/scheduling"
)

const repairPackName = "repair"

// repairPack finds what is wrong with a campaign — its calendar, its brief
// against its posts — and offers each fix as a button, not a report.
func repairPack(d Deps) assistant.Pack {
	return assistant.Pack{
		Name:    repairPackName,
		Summary: "check a campaign's calendar and its brief and posts for problems, and fix them: even out the dates, fill silences, finish what's due",
		Screens: []viewcontext.Screen{"calendar", "campaign.calendar", "campaign.overview"},
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			check := genkit.DefineTool(g, "checkCalendar",
				"Checks a campaign's calendar: posts outside its dates or on days it doesn't publish, crowded days, long silences, and posts due soon that aren't written or ready. Each finding comes with its fixes, offered for the user to confirm.",
				assistantkit.Wrap(func(ctx context.Context, in CampaignRef) (*CheckCalendarOutput, error) {
					return checkCalendar(ctx, d, in)
				}))
			optimise := genkit.DefineTool(g, "optimiseCalendar",
				"Works out how to even a campaign's movable posts (planned, draft, ready) out over its dates, phase by phase, and offers the moves for the user to confirm, with each post's old and new date.",
				assistantkit.Wrap(func(ctx context.Context, in CampaignRef) (*OptimiseOutput, error) {
					return optimiseCalendar(ctx, d, in)
				}))
			apply := genkit.DefineTool(g, "applyCalendarMoves",
				"Moves posts to new dates exactly as offered. Only runs once the user confirmed the offer.",
				assistantkit.Gated("applyCalendarMoves", assistantkit.Gate[ApplyMovesInput]{
					Summary: func(in ApplyMovesInput) string { return fmt.Sprintf("Move %s", plural(len(in.Moves), "post", "posts")) },
				}, assistantkit.Wrap(func(ctx context.Context, in ApplyMovesInput) (*ApplyMovesOutput, error) {
					return applyMoves(ctx, d, in)
				})))
			dismiss := genkit.DefineTool(g, "dismissFinding",
				"Dismisses a finding the user is fine with, so checks of this thread stop raising it while its posts stay the same.",
				assistantkit.Wrap(func(ctx context.Context, in DismissFindingInput) (*DismissFindingOutput, error) {
					return dismissFinding(ctx, d, in)
				}))
			specs := []assistantkit.Spec{
				spec(repairPackName, check, assistantkit.TierRead,
					`Call this when the user asks what's wrong with the calendar or plan, or to check it — "anything I should fix?", "is the calendar ok?". Relay the findings in order and offer their fixes, which are already offered (proposalId) or are a post to open (navigate to it). Don't fix anything they didn't pick.`),
				spec(repairPackName, optimise, assistantkit.TierRead,
					`Call this when the user asks to even out, rebalance or optimise the calendar. It offers the moves; show a few of them (old → new date) and say they run once the user confirms.`),
				spec(repairPackName, apply, assistantkit.TierProposal,
					`Never call this yourself: it is what an optimise or check offer runs once confirmed.`),
				spec(repairPackName, dismiss, assistantkit.TierRead,
					`Call this when the user says a finding is fine as it is ("leave the gap", "that's intended"). Pass its key.`),
			}
			if d.CheckBrief != nil && d.CheckPosts != nil {
				review := genkit.DefineTool(g, "reviewCampaign",
					"Reviews a campaign's brief for consistency and completeness, and its unpublished posts against the brief, and offers a fix for each finding.",
					assistantkit.Wrap(func(ctx context.Context, in CampaignRef) (*ReviewOutput, error) { return reviewCampaign(ctx, d, in) }))
				specs = append(specs, spec(repairPackName, review, assistantkit.TierRead,
					`Call this when the user asks whether the brief holds up or the posts follow it, or for a full review. Relay the findings with their fixes: a brief finding's fix improves the brief (offered), a post's opens the post.`))
			}
			return specs, nil
		},
	}
}

// CampaignRef names a campaign; empty is the thread's.
type CampaignRef struct {
	CampaignID string `json:"campaignId,omitempty" jsonschema:"description=Defaults to the thread's campaign"`
}

func (d Deps) loadCampaign(ctx context.Context, ref CampaignRef) (*models.Campaign, error) {
	id := strings.TrimSpace(ref.CampaignID)
	if id == "" {
		id = assistant.CampaignID(ctx)
	}
	if id == "" {
		return nil, assistantkit.Refused("which campaign? pass campaignId")
	}
	c, err := d.Campaigns.GetByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && c.DeletedAt != nil) {
		return nil, assistantkit.Refused("that campaign doesn't exist")
	}
	return c, err
}

// FixOffer is a finding's fix, as the assistant offers it.
type FixOffer struct {
	calendarcheck.Fix
	// ProposalID is the offer that applies the fix; empty for a post to
	// open, which navigate does.
	ProposalID string `json:"proposalId,omitempty"`
}

// FindingOut is a finding with its fixes offered.
type FindingOut struct {
	Key      string     `json:"key"`
	Code     string     `json:"code"`
	Severity string     `json:"severity"`
	Message  string     `json:"message"`
	PostIDs  []string   `json:"postIds,omitempty"`
	Fixes    []FixOffer `json:"fixes"`
}

// CheckCalendarOutput is the campaign's open findings.
type CheckCalendarOutput struct {
	CampaignID string       `json:"campaignId"`
	Findings   []FindingOut `json:"findings"`
	// Dismissed counts findings left out because the user dismissed them.
	Dismissed int `json:"dismissed,omitempty"`
}

func checkCalendar(ctx context.Context, d Deps, in CampaignRef) (*CheckCalendarOutput, error) {
	c, err := d.loadCampaign(ctx, in)
	if err != nil {
		return nil, err
	}
	posts, err := d.Posts.ListByCampaign(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	dismissed := map[string]bool{}
	if d.Dismissals != nil {
		scope, _ := assistant.Scope(ctx)
		if dismissed, err = d.Dismissals.Keys(ctx, scope.Kind, scope.ID); err != nil {
			return nil, err
		}
	}
	out := &CheckCalendarOutput{CampaignID: c.ID, Findings: []FindingOut{}}
	// One redistribution fixes every finding that offers it.
	var redistributeID string
	for _, f := range calendarcheck.Check(c, posts, time.Now()) {
		if dismissed[f.Key] {
			out.Dismissed++
			continue
		}
		fo := FindingOut{Key: f.Key, Code: f.Code, Severity: string(f.Severity), Message: f.Message, PostIDs: f.PostIDs, Fixes: []FixOffer{}}
		for _, fix := range f.Fixes {
			offer, err := offerFix(ctx, d, c, posts, fix, &redistributeID)
			if err != nil {
				return nil, err
			}
			fo.Fixes = append(fo.Fixes, offer)
		}
		out.Findings = append(out.Findings, fo)
	}
	assistant.Emit(ctx, "calendar_findings", out)
	return out, nil
}

// offerFix makes a fix's offer: one shared redistribution (its id kept in
// redistributeID), or planning ideas for a silence. A post to open needs
// no offer.
func offerFix(ctx context.Context, d Deps, c *models.Campaign, posts []models.Post, fix calendarcheck.Fix, redistributeID *string) (FixOffer, error) {
	offer := FixOffer{Fix: fix}
	var err error
	switch fix.Kind {
	case calendarcheck.FixRedistribute:
		if *redistributeID == "" {
			*redistributeID, err = offerMoves(ctx, c, posts)
		}
		offer.ProposalID = *redistributeID
	case calendarcheck.FixPlanWindow:
		if d.Campaign != nil {
			offer.ProposalID, err = assistantkit.Offer(ctx, assistantkit.Proposal{
				Tool: "generatePosts",
				Args: campaign_assistant.GeneratePostsInput{CampaignID: c.ID, Platforms: platformNames(c),
					Count: gapIdeas, WindowStart: fix.From, WindowEnd: fix.To},
				Summary: fmt.Sprintf("Plan %d ideas for %s to %s", gapIdeas, fix.From, fix.To),
			})
		}
	}
	return offer, err
}

// Move is one post's new date.
type Move struct {
	PostID string `json:"postId"`
	Title  string `json:"title,omitempty"`
	// From is the post's date when offered, YYYY-MM-DD in the campaign's
	// timezone or empty; To its new one.
	From string `json:"from,omitempty"`
	To   string `json:"to"`
	// FromAt is the post's exact scheduled_at when offered; the move applies
	// only while the post still has it.
	FromAt *time.Time `json:"fromAt,omitempty"`
}

// moves is how reschedule would even the campaign out, as moves; dates are
// the campaign's local days.
func moves(c *models.Campaign, posts []models.Post) []Move {
	byID := make(map[string]models.Post, len(posts))
	for _, p := range posts {
		byID[p.ID] = p
	}
	loc := calendarcheck.Location(c)
	var out []Move
	for _, a := range reschedule.Plan(c, posts) {
		p := byID[a.PostID]
		to := a.ScheduledAt.Format(time.DateOnly)
		from := ""
		if p.ScheduledAt != nil {
			from = p.ScheduledAt.In(loc).Format(time.DateOnly)
		}
		if from == to {
			continue
		}
		out = append(out, Move{PostID: p.ID, Title: p.Title, From: from, To: to, FromAt: p.ScheduledAt})
	}
	return out
}

// offerMoves offers evening the campaign out; "" when nothing would move.
func offerMoves(ctx context.Context, c *models.Campaign, posts []models.Post) (string, error) {
	ms := moves(c, posts)
	if len(ms) == 0 {
		return "", nil
	}
	return assistantkit.Offer(ctx, assistantkit.Proposal{
		Tool: "applyCalendarMoves", Args: ApplyMovesInput{CampaignID: c.ID, Moves: ms},
		Summary: fmt.Sprintf("Move %s to even out %q", plural(len(ms), "post", "posts"), c.Name),
	})
}

// OptimiseOutput is the offered moves.
type OptimiseOutput struct {
	CampaignID string `json:"campaignId"`
	ProposalID string `json:"proposalId,omitempty"`
	Moves      []Move `json:"moves"`
	Note       string `json:"note,omitempty"`
}

func optimiseCalendar(ctx context.Context, d Deps, in CampaignRef) (*OptimiseOutput, error) {
	c, err := d.loadCampaign(ctx, in)
	if err != nil {
		return nil, err
	}
	if c.StartDate == nil || c.EndDate == nil {
		return nil, assistantkit.Refused("the campaign has no start and end dates to spread posts over; set them first")
	}
	posts, err := d.Posts.ListByCampaign(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	out := &OptimiseOutput{CampaignID: c.ID, Moves: moves(c, posts)}
	if len(out.Moves) == 0 {
		out.Note = "the calendar is already even; nothing would move"
		return out, nil
	}
	if out.ProposalID, err = offerMoves(ctx, c, posts); err != nil {
		return nil, err
	}
	return out, nil
}

// ApplyMovesInput is the moves an offer applies.
type ApplyMovesInput struct {
	CampaignID string `json:"campaignId"`
	Moves      []Move `json:"moves"`
}

// ApplyMovesOutput is what moved, and what had changed since the offer.
type ApplyMovesOutput struct {
	Moved   int      `json:"moved"`
	Skipped []string `json:"skipped,omitempty"`
}

// applyMoves moves the offered posts that still have the schedule they were
// offered with, onto dates the campaign still allows; the rest are skipped.
func applyMoves(ctx context.Context, d Deps, in ApplyMovesInput) (*ApplyMovesOutput, error) {
	c, err := d.loadCampaign(ctx, CampaignRef{CampaignID: in.CampaignID})
	if err != nil {
		return nil, err
	}
	posts, err := d.Posts.ListByCampaign(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*models.Post, len(posts))
	for i := range posts {
		byID[posts[i].ID] = &posts[i]
	}
	out := &ApplyMovesOutput{}
	var batch []repository.ScheduleMove
	for _, m := range in.Moves {
		p := byID[m.PostID]
		// A post moved, scheduled or deleted since the offer keeps its date,
		// and so does one whose new date the campaign no longer allows.
		at, ok := moveTarget(c, m)
		if p == nil || !reschedule.Eligible(p.Status) || !sameInstant(p.ScheduledAt, m.FromAt) || !ok {
			out.Skipped = append(out.Skipped, m.PostID)
			continue
		}
		batch = append(batch, repository.ScheduleMove{PostID: p.ID, From: m.FromAt, To: at})
	}
	if len(batch) == 0 {
		return out, nil
	}
	// The write re-checks each post's schedule, for an edit racing this one.
	moved, err := d.Posts.MoveScheduledAt(ctx, batch, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("move posts: %w", err)
	}
	for _, m := range batch {
		if !slices.Contains(moved, m.PostID) {
			out.Skipped = append(out.Skipped, m.PostID)
		}
	}
	if len(moved) > 0 {
		assistantkit.Touch(ctx, "post", "")
	}
	out.Moved = len(moved)
	return out, nil
}

// moveTarget is the instant a move lands on: its date, at the campaign's
// publishing time in its timezone. ok is false when the date doesn't parse,
// or the campaign as it is now has it outside its dates or on a day it
// doesn't publish.
func moveTarget(c *models.Campaign, m Move) (time.Time, bool) {
	date, err := time.Parse(time.DateOnly, m.To)
	if err != nil || !scheduling.EnabledWeekdays(c.PublishingDays)[date.Weekday()] {
		return time.Time{}, false
	}
	if c.StartDate != nil && date.Before(campaignphase.Date(*c.StartDate)) ||
		c.EndDate != nil && date.After(campaignphase.Date(*c.EndDate)) {
		return time.Time{}, false
	}
	at, _, _ := scheduling.ComposeScheduledAt(m.To, m.PostID, calendarcheck.Location(c),
		c.PublishingTime, c.PublishingDays, c.SpreadMinutes, c.StartDate, c.EndDate)
	if at == nil {
		return time.Time{}, false
	}
	return *at, true
}

// sameInstant reports whether two optional instants are both unset or equal.
func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// DismissFindingInput names the finding to dismiss.
type DismissFindingInput struct {
	Key string `json:"key"`
}

// DismissFindingOutput confirms it.
type DismissFindingOutput struct {
	Dismissed bool `json:"dismissed"`
}

func dismissFinding(ctx context.Context, d Deps, in DismissFindingInput) (*DismissFindingOutput, error) {
	if d.Dismissals == nil {
		return nil, assistantkit.Refused("findings can't be dismissed here")
	}
	if strings.TrimSpace(in.Key) == "" {
		return nil, assistantkit.Refused("pass the finding's key")
	}
	scope, _ := assistant.Scope(ctx)
	if err := d.Dismissals.Dismiss(ctx, scope.Kind, scope.ID, in.Key, actor(ctx)); err != nil {
		return nil, err
	}
	return &DismissFindingOutput{Dismissed: true}, nil
}

// ReviewFinding is a brief or post finding with its fix.
type ReviewFinding struct {
	// Kind is brief or post.
	Kind       string   `json:"kind"`
	Severity   string   `json:"severity"`
	Aspect     string   `json:"aspect,omitempty"`
	PostID     string   `json:"postId,omitempty"`
	Title      string   `json:"title,omitempty"`
	Issue      string   `json:"issue"`
	Suggestion string   `json:"suggestion,omitempty"`
	Fix        FixOffer `json:"fix"`
}

// ReviewOutput is the campaign's review.
type ReviewOutput struct {
	CampaignID   string          `json:"campaignId"`
	BriefSummary string          `json:"briefSummary,omitempty"`
	PostsSummary string          `json:"postsSummary,omitempty"`
	PostsChecked int             `json:"postsChecked"`
	Findings     []ReviewFinding `json:"findings"`
}

func reviewCampaign(ctx context.Context, d Deps, in CampaignRef) (*ReviewOutput, error) {
	c, err := d.loadCampaign(ctx, in)
	if err != nil {
		return nil, err
	}
	brief, err := d.CheckBrief(ctx, c.ID, nil)
	if err != nil {
		return nil, assistantkit.Refused("couldn't review the brief: " + err.Error())
	}
	postsReview, err := d.CheckPosts(ctx, consistency.PostsCheckRequest{CampaignID: c.ID}, nil)
	if err != nil {
		return nil, assistantkit.Refused("couldn't review the posts: " + err.Error())
	}
	out := &ReviewOutput{CampaignID: c.ID, BriefSummary: brief.Summary, PostsSummary: postsReview.Summary,
		PostsChecked: postsReview.Checked, Findings: []ReviewFinding{}}
	var briefFix string
	for _, f := range brief.Findings {
		// One brief edit takes every brief finding.
		if briefFix == "" && d.Campaign != nil {
			if briefFix, err = assistantkit.Offer(ctx, assistantkit.Proposal{
				Tool:    "enrichBrief",
				Args:    campaign_assistant.EnrichBriefInput{CampaignID: c.ID, Instruction: "Fix these review findings: " + briefIssues(brief.Findings)},
				Summary: "Improve the brief to fix the review's findings",
			}); err != nil {
				return nil, err
			}
		}
		out.Findings = append(out.Findings, ReviewFinding{Kind: "brief", Severity: string(f.Severity), Aspect: string(f.Aspect),
			Issue: f.Issue, Suggestion: f.Suggestion, Fix: FixOffer{Fix: calendarcheck.Fix{Kind: "improve_brief", Title: "Improve the brief"}, ProposalID: briefFix}})
	}
	for _, f := range postsReview.Findings {
		out.Findings = append(out.Findings, ReviewFinding{Kind: "post", Severity: string(f.Severity), PostID: f.PostID, Title: f.Title,
			Issue: f.Issue, Suggestion: f.Suggestion, Fix: FixOffer{Fix: calendarcheck.Fix{Kind: calendarcheck.FixOpenPost, Title: "Open the post", PostID: f.PostID}}})
	}
	return out, nil
}

// gapIdeas is how many ideas a silence's fix plans.
const gapIdeas = 3

// platformNames are the platforms the campaign targets, by name.
func platformNames(c *models.Campaign) []string {
	out := make([]string, len(c.Platforms))
	for i, p := range c.Platforms {
		out[i] = p.Name
	}
	return out
}

func briefIssues(fs []consistency.Finding) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = fmt.Sprintf("%s: %s (%s)", f.Aspect, f.Issue, f.Suggestion)
	}
	return strings.Join(parts, "; ")
}
