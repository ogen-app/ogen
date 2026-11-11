package packs

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/campaignphase"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/calendarcheck"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/platforms"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/reschedule"
	"github.com/ogen-app/ogen/src/usecase/scheduling"
)

const repairPackName = "repair"

// repairPack finds what is wrong with a campaign — its calendar, its brief
// against its posts — and offers each fix as a button, not a report.
func repairPack(d Deps) assistant.Pack {
	return assistant.Pack{
		Name:    repairPackName,
		Summary: "check a campaign's calendar, its brief and posts, and posts against the brand voice and guardrails, for problems, and fix them: even out the dates, move a post, fill silences and goal shortfalls, finish what's due, change, unschedule or delete posts of a type the campaign dropped",
		Screens: []viewcontext.Screen{"calendar", "campaign.calendar", "campaign.overview", "campaign.list"},
		// Each tool's last output reaches the client as
		// complete.results.repair.<tool>, so findings, their keys and
		// fixes, the locked posts and a fix's undo id replay from history.
		Begin: beginResults(repairPackName),
		End:   endResults(repairPackName),
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			check := genkit.DefineTool(g, "checkCalendar",
				"Checks a campaign's calendar: posts outside its dates or on days it doesn't publish, crowded days and slots, long silences, goal periods short of posts, posts due soon that aren't written or ready, posts of a type the campaign no longer includes, and posts from one idea or source published close together. Each finding comes with its fixes, offered for the user to confirm.",
				assistantkit.Wrap(recorded(repairPackName, "checkCalendar", nil, func(ctx context.Context, in CampaignRef) (*CheckCalendarOutput, error) {
					return checkCalendar(ctx, d, in)
				})), ai.WithStrictSchema(false))
			optimise := genkit.DefineTool(g, "optimiseCalendar",
				"Works out how to even a campaign's movable posts out over its dates, phase by phase, and offers the moves for the user to confirm, with each post's old and new date. Planned, draft and ready posts move: nothing is queued to publish for them yet. Scheduled posts stay where they are and come back as locked.",
				assistantkit.Wrap(recorded(repairPackName, "optimiseCalendar", nil, func(ctx context.Context, in CampaignRef) (*OptimiseOutput, error) {
					return optimiseCalendar(ctx, d, in)
				})), ai.WithStrictSchema(false))
			apply := genkit.DefineTool(g, "applyCalendarMoves",
				"Moves posts to new dates exactly as offered. Only runs once the user confirmed the offer.",
				assistantkit.Gated("applyCalendarMoves", assistantkit.Gate[ApplyMovesInput]{
					Recheck: func(ctx context.Context, in ApplyMovesInput) (ApplyMovesInput, error) {
						return movesThatHold(ctx, d, in)
					},
					Summary: func(in ApplyMovesInput) string { return fmt.Sprintf("Move %s", plural(len(in.Moves), "post", "posts")) },
				}, assistantkit.Wrap(recorded(repairPackName, "applyCalendarMoves", nil, func(ctx context.Context, in ApplyMovesInput) (*ApplyMovesOutput, error) {
					return applyMoves(ctx, d, in)
				}))), ai.WithStrictSchema(false))
			dismiss := genkit.DefineTool(g, "dismissFinding",
				"Dismisses a calendar or review finding the user is fine with, for its campaign: no check, here or on the campaign's Overview, raises it again while its key stays the same. A calendar finding's key follows its posts; a review finding's follows the brief, or the post's text.",
				assistantkit.Wrap(func(ctx context.Context, in DismissFindingInput) (*DismissFindingOutput, error) {
					return dismissFinding(ctx, d, in)
				}), ai.WithStrictSchema(false))
			specs := []assistantkit.Spec{
				spec(repairPackName, check, assistantkit.TierRead,
					`Call this when the user asks what's wrong with the calendar or plan, or to check it — "anything I should fix?", "is the calendar ok?". Relay the findings in order and offer their fixes, which are already offered (proposalId) or are a post to open (navigate to it). Don't fix anything they didn't pick. When the user asks to fix one problem by its key (as the campaign's Overview does), check and offer only that finding's fixes.`),
				spec(repairPackName, optimise, assistantkit.TierRead,
					`Call this when the user asks to even out, rebalance or optimise the calendar. It offers the moves; show a few of them (old → new date), say that the scheduled posts stay locked in place, and that the moves run once the user confirms.`),
				spec(repairPackName, apply, assistantkit.TierProposal,
					`Never call this yourself: it is what an optimise or check offer runs once confirmed.`),
				spec(repairPackName, dismiss, assistantkit.TierRead,
					`Call this when the user says a finding is fine as it is ("leave the gap", "that's intended"). Pass its key, and the campaign the check was for when it isn't the thread's.`),
			}
			specs = append(specs, postFixSpecs(g, d)...)
			if d.CheckBrief != nil && d.CheckPosts != nil {
				review := genkit.DefineTool(g, "reviewCampaign",
					"Reviews a campaign's brief for consistency and completeness, and its unpublished posts against the brief, and offers fixes for each finding.",
					assistantkit.Wrap(recorded(repairPackName, "reviewCampaign", nil, func(ctx context.Context, in CampaignRef) (*ReviewOutput, error) { return reviewCampaign(ctx, d, in) })), ai.WithStrictSchema(false))
				specs = append(specs, spec(repairPackName, review, assistantkit.TierRead,
					`Call this when the user asks whether the brief holds up or the posts follow it, or for a full review. Relay the findings with their fixes, which are already offered (proposalId) or open the post (navigate to it): a brief finding's fixes improve the brief, a post's rewrite it to follow the brief. A finding the user is fine with is dismissed by its key.`))
			}
			if d.Revise != nil {
				revise := genkit.DefineTool(g, "revisePost",
					"Rewrites one of the campaign's posts by an instruction, as the post's own assistant would, and saves it as a new version. It is offered for the user to confirm before it runs.",
					assistantkit.Gated("revisePost", assistantkit.Gate[RevisePostInput]{
						Summary: func(in RevisePostInput) string { return "Rewrite the post: " + in.Instruction },
					}, assistantkit.Wrap(recorded(repairPackName, "revisePost", nil, func(ctx context.Context, in RevisePostInput) (*RevisePostOutput, error) {
						return revisePost(ctx, d, in)
					}))),
					ai.WithStrictSchema(false))
				specs = append(specs, spec(repairPackName, revise, assistantkit.TierProposal,
					`Call this when the user, in a campaign's thread, asks to rewrite one of its posts by an instruction; pass the post's id and the instruction verbatim. It is offered for the user to confirm. A review finding's "rewrite it" fix is already this offer. In the post's own thread, use editPost instead.`))
			}
			if d.CheckBrand != nil {
				brand := genkit.DefineTool(g, "checkBrand",
					"Checks posts against the brand they're written for: the voice and its channel notes, banned words, claims the guardrails rule out, claims no fact backs, and a missing disclaimer. Takes the posts named, else the thread's post, else a campaign's unpublished posts. Each finding quotes the post and opens it to fix.",
					assistantkit.Wrap(recorded(repairPackName, "checkBrand", nil, func(ctx context.Context, in CheckBrandInput) (*CheckBrandOutput, error) {
						return checkBrand(ctx, d, in)
					})), ai.WithStrictSchema(false))
				specs = append(specs, spec(repairPackName, brand, assistantkit.TierRead,
					`Call this when the user asks whether posts are on brand, in the voice, or within the guardrails — "are these on brand?", "check the selected posts against our voice". Pass the selected posts' ids when there is a selection. Relay the findings, most serious first, with the quoted words; each one's fix opens the post. In the post's own thread, offer to fix it with editPost.`))
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
	var ideas map[string][]string
	if d.Links != nil {
		if ideas, err = d.Links.IdeasOfCampaignPosts(ctx, c.ID); err != nil {
			return nil, fmt.Errorf("read the posts' ideas: %w", err)
		}
	}
	dismissed, err := d.dismissedKeys(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	out := &CheckCalendarOutput{CampaignID: c.ID, Findings: []FindingOut{}}
	// One redistribution fixes every finding that offers it.
	var redistributeID string
	for _, f := range calendarcheck.Check(c, posts, ideas, time.Now()) {
		if dismissed[f.Key] {
			out.Dismissed++
			continue
		}
		fo := FindingOut{Key: f.Key, Code: f.Code, Severity: string(f.Severity), Message: f.Message, PostIDs: f.PostIDs, Fixes: []FixOffer{}}
		for _, fix := range f.Fixes {
			offer, ok, err := offerFix(ctx, d, c, posts, fix, &redistributeID)
			if err != nil {
				return nil, err
			}
			if ok {
				fo.Fixes = append(fo.Fixes, offer)
			}
		}
		out.Findings = append(out.Findings, fo)
	}
	assistant.Emit(ctx, "calendar_findings", out)
	return out, nil
}

// offerFix makes a fix's offer: one shared redistribution (its id kept in
// redistributeID), planning ideas for a silence, or the tool call that
// makes the fix. A post to open needs no offer. ok is false for a fix this
// assistant can't make, which is left out.
func offerFix(ctx context.Context, d Deps, c *models.Campaign, posts []models.Post, fix calendarcheck.Fix, redistributeID *string) (FixOffer, bool, error) {
	offer := FixOffer{Fix: fix}
	var err error
	propose := func(tool string, args any) {
		offer.ProposalID, err = assistantkit.Offer(ctx, assistantkit.Proposal{Tool: tool, Args: args, Summary: fix.Title})
	}
	switch fix.Kind {
	case calendarcheck.FixRedistribute:
		if *redistributeID == "" {
			*redistributeID, err = offerMoves(ctx, c, posts)
		}
		offer.ProposalID = *redistributeID
	case calendarcheck.FixPlanWindow:
		switch {
		case d.Campaign == nil:
		case len(c.Platforms) == 0:
			// Ideas are planned for a platform, and the campaign has none.
			offer.Title = "Add a target platform to the campaign, then plan ideas for these dates"
		default:
			offer.ProposalID, err = offerGapIdeas(ctx, d, c, fix)
		}
	case calendarcheck.FixMovePost:
		i := slices.IndexFunc(posts, func(p models.Post) bool { return p.ID == fix.PostID })
		if i < 0 {
			return offer, false, nil
		}
		propose("applyCalendarMoves", ApplyMovesInput{CampaignID: c.ID, Moves: []Move{
			{PostID: fix.PostID, Title: posts[i].Title, From: fix.From, To: fix.To, FromAt: posts[i].ScheduledAt}}})
	case calendarcheck.FixEnablePostType:
		if d.Campaign == nil {
			return offer, false, nil
		}
		propose("updateCampaignPlatforms", campaign_assistant.UpdateCampaignPlatformsInput{
			Platforms: []platforms.Request{{Platform: fix.PlatformID, PostTypes: []string{fix.PostType}}}})
	case calendarcheck.FixEnablePlatform:
		if d.Campaign == nil {
			return offer, false, nil
		}
		propose("updateCampaignPlatforms", campaign_assistant.UpdateCampaignPlatformsInput{
			Platforms: []platforms.Request{{Platform: fix.PlatformID}}})
	case calendarcheck.FixConvertPostType:
		propose("changePostType", ChangePostTypeInput{CampaignID: c.ID, PostIDs: fix.PostIDs, PostType: fix.PostType})
	case calendarcheck.FixUnschedule:
		propose("unschedulePosts", PostsRef{CampaignID: c.ID, PostIDs: fix.PostIDs})
	case calendarcheck.FixDelete:
		if d.Remove == nil {
			return offer, false, nil
		}
		propose("deletePosts", PostsRef{CampaignID: c.ID, PostIDs: fix.PostIDs})
	}
	return offer, true, err
}

// offerGapIdeas offers planning ideas for a silence, in the phase the
// silence starts in and ending with that phase. A campaign with no ready
// sources gets the choice the campaign's own tool offers: add a source
// first, or go ahead from general knowledge.
func offerGapIdeas(ctx context.Context, d Deps, c *models.Campaign, fix calendarcheck.Fix) (string, error) {
	phase, end := gapPhase(c, fix.From, fix.To)
	count := cmp.Or(fix.Count, gapIdeas)
	in := campaign_assistant.GeneratePostsInput{CampaignID: c.ID, Platforms: platformNames(c), Phase: phase,
		Count: count, WindowStart: fix.From, WindowEnd: end}
	p := assistantkit.Proposal{Tool: "generatePosts", Args: in,
		Summary: fmt.Sprintf("Plan %d ideas for %s to %s", count, fix.From, end)}
	if d.Assets != nil {
		ready, _, err := flowkit.ReadyCampaignAssetIDs(ctx, d.Assets, c)
		if err != nil {
			return "", fmt.Errorf("count campaign sources: %w", err)
		}
		if len(ready) == 0 {
			exploratory := in
			exploratory.Exploratory = true
			p.Summary = "This campaign has no sources yet, so these ideas would come from general knowledge, not your material. " +
				"Add a document or a link on the campaign's Assets tab, or go ahead without them."
			p.Options = []assistantkit.Option{{ID: "exploratory", Title: "Go ahead without sources (marked not grounded)", Tool: "generatePosts", Args: exploratory}}
		}
	}
	return assistantkit.Offer(ctx, p)
}

// gapPhase is the phase a silence from..to starts in, by id, and the
// silence's end cut at that phase's end, so no idea is dated into the next
// phase. An unparsable date or a campaign without phases leaves both to the
// tool.
func gapPhase(c *models.Campaign, from, to string) (phaseID, end string) {
	start, err := time.Parse(time.DateOnly, from)
	if err != nil {
		return "", to
	}
	windows, _ := campaignphase.Resolve(c)
	for _, w := range windows {
		if !w.Contains(start) {
			continue
		}
		if last, err := time.Parse(time.DateOnly, to); err == nil && last.After(w.End) {
			to = w.End.Format(time.DateOnly)
		}
		return w.Phase.ID, to
	}
	if ph, ok := campaignphase.PhaseAt(c, start); ok {
		return ph.ID, to
	}
	return "", to
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

// OptimiseOutput is the offered moves, and the scheduled posts they leave
// where they are.
type OptimiseOutput struct {
	CampaignID string       `json:"campaignId"`
	ProposalID string       `json:"proposalId,omitempty"`
	Moves      []Move       `json:"moves"`
	Locked     []LockedPost `json:"locked"`
	Note       string       `json:"note,omitempty"`
}

// LockedPost is a scheduled post a redistribution doesn't move: a copy of it
// waits to publish.
type LockedPost struct {
	PostID string `json:"postId"`
	Title  string `json:"title,omitempty"`
	// Date is its day in the campaign's timezone, YYYY-MM-DD.
	Date string `json:"date"`
}

// locked are the campaign's scheduled posts, by date.
func locked(c *models.Campaign, posts []models.Post) []LockedPost {
	loc := calendarcheck.Location(c)
	out := []LockedPost{}
	for _, p := range posts {
		if p.ScheduledAt == nil || reschedule.Eligible(p.Status) || p.Status == models.PostStatusPublished {
			continue
		}
		out = append(out, LockedPost{PostID: p.ID, Title: p.Title, Date: p.ScheduledAt.In(loc).Format(time.DateOnly)})
	}
	slices.SortStableFunc(out, func(a, b LockedPost) int { return strings.Compare(a.Date, b.Date) })
	return out
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
	out := &OptimiseOutput{CampaignID: c.ID, Moves: moves(c, posts), Locked: locked(c, posts)}
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
		at, ok := moveHolds(c, byID[m.PostID], m)
		if !ok {
			out.Skipped = append(out.Skipped, m.PostID)
			continue
		}
		batch = append(batch, repository.ScheduleMove{PostID: m.PostID, From: m.FromAt, To: at})
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

// moveHolds is where m lands while it still holds: a post moved, scheduled
// or deleted since the offer keeps its date, and so does one whose new date
// the campaign no longer allows.
func moveHolds(c *models.Campaign, p *models.Post, m Move) (time.Time, bool) {
	at, ok := moveTarget(c, m)
	if p == nil || !reschedule.Eligible(p.Status) || !sameInstant(p.ScheduledAt, m.FromAt) || !ok {
		return time.Time{}, false
	}
	return at, true
}

// movesThatHold narrows confirmed moves to the ones that still hold; none
// is a refusal.
func movesThatHold(ctx context.Context, d Deps, in ApplyMovesInput) (ApplyMovesInput, error) {
	c, err := d.loadCampaign(ctx, CampaignRef{CampaignID: in.CampaignID})
	if err != nil {
		return in, err
	}
	posts, err := d.Posts.ListByCampaign(ctx, c.ID)
	if err != nil {
		return in, err
	}
	keep := slices.DeleteFunc(slices.Clone(in.Moves), func(m Move) bool {
		i := slices.IndexFunc(posts, func(p models.Post) bool { return p.ID == m.PostID })
		if i < 0 {
			return true
		}
		_, ok := moveHolds(c, &posts[i], m)
		return !ok
	})
	if len(keep) == 0 {
		return in, assistantkit.Refused("none of those moves hold any more: the posts were moved, scheduled or deleted since")
	}
	if len(keep) < len(in.Moves) {
		in.Moves = keep
	}
	return in, nil
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
	// CampaignID is the campaign whose check raised it.
	CampaignID string `json:"campaignId,omitempty" jsonschema:"description=The campaign whose check raised the finding; defaults to the thread's campaign"`
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
	// A finding belongs to the campaign checked, whichever thread checked
	// it, so the campaign's Overview and every thread leave it out.
	c, err := d.loadCampaign(ctx, CampaignRef{CampaignID: in.CampaignID})
	if err != nil {
		return nil, err
	}
	if err := d.Dismissals.Dismiss(ctx, models.AssistantScopeCampaign, c.ID, in.Key, actor(ctx)); err != nil {
		return nil, err
	}
	return &DismissFindingOutput{Dismissed: true}, nil
}

// ReviewFinding is a brief or post finding with its fixes.
type ReviewFinding struct {
	// Kind is brief or post.
	Kind string `json:"kind"`
	// Key dismisses the finding while the brief, or its post, stays the
	// same.
	Key        string     `json:"key"`
	Severity   string     `json:"severity"`
	Aspect     string     `json:"aspect,omitempty"`
	PostID     string     `json:"postId,omitempty"`
	Title      string     `json:"title,omitempty"`
	Issue      string     `json:"issue"`
	Suggestion string     `json:"suggestion,omitempty"`
	Fixes      []FixOffer `json:"fixes"`
}

// ReviewOutput is the campaign's review.
type ReviewOutput struct {
	CampaignID   string          `json:"campaignId"`
	BriefSummary string          `json:"briefSummary,omitempty"`
	PostsSummary string          `json:"postsSummary,omitempty"`
	PostsChecked int             `json:"postsChecked"`
	Findings     []ReviewFinding `json:"findings"`
	// Dismissed counts findings left out because the user dismissed them.
	Dismissed int `json:"dismissed,omitempty"`
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
	dismissed, err := d.dismissedKeys(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	out := &ReviewOutput{CampaignID: c.ID, BriefSummary: brief.Summary, PostsSummary: postsReview.Summary,
		PostsChecked: postsReview.Checked, Findings: []ReviewFinding{}}
	var open []consistency.Finding
	for _, f := range brief.Findings {
		if dismissed[f.Key] {
			out.Dismissed++
			continue
		}
		open = append(open, f)
	}
	briefs, err := briefFindings(ctx, d, c, open)
	if err != nil {
		return nil, err
	}
	out.Findings = append(out.Findings, briefs...)
	for _, f := range postsReview.Findings {
		if dismissed[f.Key] {
			out.Dismissed++
			continue
		}
		rf, err := postFinding(ctx, d, c, f)
		if err != nil {
			return nil, err
		}
		out.Findings = append(out.Findings, rf)
	}
	return out, nil
}

// briefFindings offers the brief's open findings their fixes: each its own
// brief edit, and when there are several, one edit taking them all.
func briefFindings(ctx context.Context, d Deps, c *models.Campaign, open []consistency.Finding) ([]ReviewFinding, error) {
	var allFix *FixOffer
	if len(open) > 1 && d.Campaign != nil {
		id, err := assistantkit.Offer(ctx, assistantkit.Proposal{
			Tool:    "enrichBrief",
			Args:    campaign_assistant.EnrichBriefInput{CampaignID: c.ID, Instruction: "Fix these review findings: " + briefIssues(open)},
			Summary: "Improve the brief to fix the review's findings",
		})
		if err != nil {
			return nil, err
		}
		allFix = &FixOffer{Fix: calendarcheck.Fix{Kind: consistency.FixImproveBrief, Title: "Fix all of the brief's findings"}, ProposalID: id}
	}
	out := make([]ReviewFinding, 0, len(open))
	for _, f := range open {
		rf := ReviewFinding{Kind: "brief", Key: f.Key, Severity: f.Severity, Aspect: f.Aspect, Issue: f.Issue, Suggestion: f.Suggestion, Fixes: []FixOffer{}}
		for _, fix := range f.Fixes {
			if fix.Kind != consistency.FixImproveBrief || d.Campaign == nil {
				continue
			}
			id, err := assistantkit.Offer(ctx, assistantkit.Proposal{Tool: "enrichBrief",
				Args: campaign_assistant.EnrichBriefInput{CampaignID: c.ID, Instruction: fix.Instruction}, Summary: fix.Title})
			if err != nil {
				return nil, err
			}
			rf.Fixes = append(rf.Fixes, FixOffer{Fix: calendarcheck.Fix{Kind: calendarcheck.FixKind(fix.Kind), Title: fix.Title}, ProposalID: id})
		}
		if allFix != nil {
			rf.Fixes = append(rf.Fixes, *allFix)
		}
		out = append(out, rf)
	}
	return out, nil
}

// postFinding offers a drifting post's fixes: rewriting it, when the
// writer is here, and opening it.
func postFinding(ctx context.Context, d Deps, c *models.Campaign, f consistency.PostFinding) (ReviewFinding, error) {
	rf := ReviewFinding{Kind: "post", Key: f.Key, Severity: f.Severity, PostID: f.PostID, Title: f.Title,
		Issue: f.Issue, Suggestion: f.Suggestion, Fixes: []FixOffer{}}
	for _, fix := range f.Fixes {
		offer := FixOffer{Fix: calendarcheck.Fix{Kind: calendarcheck.FixKind(fix.Kind), Title: fix.Title, PostID: fix.PostID}}
		if fix.Kind == consistency.FixRevisePost {
			if d.Revise == nil {
				continue
			}
			var err error
			if offer.ProposalID, err = assistantkit.Offer(ctx, assistantkit.Proposal{Tool: "revisePost",
				Args: RevisePostInput{CampaignID: c.ID, PostID: fix.PostID, Instruction: fix.Instruction}, Summary: fix.Title}); err != nil {
				return ReviewFinding{}, err
			}
		}
		rf.Fixes = append(rf.Fixes, offer)
	}
	return rf, nil
}

// dismissedKeys are the campaign's dismissed findings, the ones its Overview
// and any thread dismissed, and those dismissed in this thread before
// dismissals followed the campaign.
func (d Deps) dismissedKeys(ctx context.Context, campaignID string) (map[string]bool, error) {
	if d.Dismissals == nil {
		return map[string]bool{}, nil
	}
	keys, err := d.Dismissals.Keys(ctx, models.AssistantScopeCampaign, campaignID)
	if err != nil {
		return nil, err
	}
	scope, _ := assistant.Scope(ctx)
	if scope.Kind == models.AssistantScopeCampaign && scope.ID == campaignID {
		return keys, nil
	}
	thread, err := d.Dismissals.Keys(ctx, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	keys = maps.Clone(keys)
	maps.Copy(keys, thread)
	return keys, nil
}

// RevisePostInput is a post of the campaign and how to rewrite it.
type RevisePostInput struct {
	CampaignID  string `json:"campaignId,omitempty" jsonschema:"description=Defaults to the thread's campaign"`
	PostID      string `json:"postId"`
	Instruction string `json:"instruction"`
}

// RevisePostOutput confirms the rewrite, and whether it was saved as a
// version.
type RevisePostOutput struct {
	PostID    string `json:"postId"`
	Revised   bool   `json:"revised"`
	Versioned bool   `json:"versioned"`
	Note      string `json:"note"`
	// Grounding is the post's own: a rewrite keeps its sources. Unset when
	// the post's never was known.
	Grounding *assistantkit.Grounding `json:"grounding,omitempty"`
}

// revisePost rewrites one of the campaign's posts by the instruction a
// finding's fix carries, as the post's own assistant would, and saves a
// version the post's history restores from.
func revisePost(ctx context.Context, d Deps, in RevisePostInput) (*RevisePostOutput, error) {
	if in.Instruction == "" {
		return nil, assistantkit.Refused("say how to rewrite the post")
	}
	c, posts, skipped, err := campaignPosts(ctx, d, in.CampaignID, []string{in.PostID})
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		return nil, assistantkit.Refused("that post isn't in this campaign any more")
	}
	if posts[0].Status.IsSubmitted() {
		return nil, assistantkit.Refused("the post is " + string(posts[0].Status) + ", so its copy is locked; unschedule it first")
	}
	versioned, err := d.Revise(ctx, in.PostID, in.Instruction, "Rewritten by the campaign's assistant")
	if err != nil {
		return nil, fmt.Errorf("rewrite post: %w", err)
	}
	out := &RevisePostOutput{PostID: in.PostID, Revised: true, Versioned: versioned,
		Note:      "saved as a new version; restoring the previous version from the post's history undoes it",
		Grounding: postGrounding(ctx, d, c.ID, posts[0])}
	if !versioned {
		out.Note = "the post is rewritten, but saving the rewrite as a version failed, so its history has no copy of it"
	}
	return out, nil
}

// postGrounding is a post's grounding: its sources, with the campaign's
// analysis of each, when it was grounded; nil when that was never known.
func postGrounding(ctx context.Context, d Deps, campaignID string, p models.Post) *assistantkit.Grounding {
	if p.GroundingMode == nil {
		return nil
	}
	if *p.GroundingMode != models.GroundingGrounded {
		return new(assistantkit.GroundingFrom(nil))
	}
	g := newAssetTitles(d).grounding(ctx, fromIdeas(p.UsedAssetIDs, nil))
	return new(analysed(ctx, d, &campaignID, g))
}

// analysed names each source's analysis for a lens (campaignID nil for the
// workspace's), when it has a finished one.
func analysed(ctx context.Context, d Deps, campaignID *string, g assistantkit.Grounding) assistantkit.Grounding {
	if d.Analyses == nil {
		return g
	}
	return g.WithAnalyses(func(ids []string) (map[string]string, error) {
		return repository.ReadyAnalysisIDs(ctx, d.Analyses, ids, campaignID)
	})
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
