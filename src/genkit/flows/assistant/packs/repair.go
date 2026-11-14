package packs

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/calendarcheck"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/platforms"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/reschedule"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
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
					Recheck: func(ctx context.Context, in ApplyMovesInput) (ApplyMovesInput, []string, error) {
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
			restore := genkit.DefineTool(g, "restoreFinding",
				"Takes back a dismissal of a calendar or review finding, so checks raise it again.",
				assistantkit.Wrap(func(ctx context.Context, in DismissFindingInput) (*RestoreFindingOutput, error) {
					return restoreFinding(ctx, d, in)
				}), ai.WithStrictSchema(false))
			specs := []assistantkit.Spec{
				spec(repairPackName, check, assistantkit.TierRead,
					`Call this when the user asks what's wrong with the calendar or plan, or to check it — "anything I should fix?", "is the calendar ok?". Relay the findings in order and offer their fixes, which are already offered (proposalId) or are a post to open (navigate to it). A fix moving one post offers its free slots as options; the card shows them, so name the nearest and say the others are there. Don't fix anything they didn't pick. When the user asks to fix one problem by its key (as the campaign's Overview does), check and offer only that finding's fixes.`),
				spec(repairPackName, optimise, assistantkit.TierRead,
					`Call this when the user asks to even out, rebalance or optimise the calendar. It offers the moves; show a few of them (old → new date), say that the scheduled posts stay locked in place, and that the moves run once the user confirms.`),
				spec(repairPackName, apply, assistantkit.TierProposal,
					`Never call this yourself: it is what an optimise or check offer runs once confirmed.`),
				spec(repairPackName, dismiss, assistantkit.TierRead,
					`Call this when the user says a finding is fine as it is ("leave the gap", "that's intended"). Pass its key, and the campaign the check was for when it isn't the thread's.`),
				spec(repairPackName, restore, assistantkit.TierRead,
					`Call this when the user wants a dismissed finding back ("bring back the gap", "restore it"); a check's dismissedFindings carry their keys. Pass its key, and the campaign the check was for when it isn't the thread's.`),
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
					`Call this when the user, in a campaign's thread, asks to rewrite one of its posts by an instruction; pass the post's id and the instruction verbatim. It is offered for the user to confirm. A review or brand-check finding's "rewrite it" fix is already this offer. In the post's own thread, use editPost instead.`))
			}
			if d.CheckBrand != nil {
				brand := genkit.DefineTool(g, "checkBrand",
					"Checks posts against the brand they're written for: the voice and its channel notes, banned words, claims the guardrails rule out, claims no fact backs, and a missing disclaimer. Takes the posts named, else the thread's post, else a campaign's unpublished posts. Each finding quotes the post and opens it to fix.",
					assistantkit.Wrap(recorded(repairPackName, "checkBrand", nil, func(ctx context.Context, in CheckBrandInput) (*CheckBrandOutput, error) {
						return checkBrand(ctx, d, in)
					})), ai.WithStrictSchema(false))
				specs = append(specs, spec(repairPackName, brand, assistantkit.TierRead,
					`Call this when the user asks whether posts are on brand, in the voice, or within the guardrails — "are these on brand?", "check the selected posts against our voice". Pass the selected posts' ids when there is a selection. Relay the findings, most serious first, with the quoted words, and say how many posts were clean. A finding's "rewrite it" fix is already offered for the user to confirm; don't offer it again. In the post's own thread, offer to fix it with editPost instead.`))
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

// FixOffer is a finding's fix with the offer that applies it.
type FixOffer = campaign_assistant.FixOffer

// FindingOut is a finding with its fixes offered.
type FindingOut struct {
	calendarcheck.Finding
	Fixes []FixOffer `json:"fixes"`
}

// CheckCalendarOutput is the campaign's open findings.
type CheckCalendarOutput struct {
	CampaignID string       `json:"campaignId"`
	Findings   []FindingOut `json:"findings"`
	// Dismissed counts findings left out because the user dismissed them;
	// DismissedFindings are those findings, without their fixes, for the
	// card to show and restore.
	Dismissed         int          `json:"dismissed,omitempty"`
	DismissedFindings []FindingOut `json:"dismissedFindings"`
	// CheckedAt is when the calendar was read.
	CheckedAt time.Time `json:"checkedAt"`
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
	dismissed, err := campaign_assistant.DismissedKeys(ctx, d.Dismissals, c.ID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := &CheckCalendarOutput{CampaignID: c.ID, Findings: []FindingOut{}, DismissedFindings: []FindingOut{}, CheckedAt: now}
	// One redistribution fixes every finding that offers it.
	var redistributeID string
	for _, f := range calendarcheck.Check(c, posts, ideas, now) {
		fo := FindingOut{Finding: f, Fixes: []FixOffer{}}
		if dismissed[f.Key] {
			out.Dismissed++
			out.DismissedFindings = append(out.DismissedFindings, fo)
			continue
		}
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
	// The offer's card and options travel on the fix too, so the finding's
	// row can draw them.
	propose := func(tool string, args any, card *assistantkit.Card, options ...assistantkit.Option) {
		offer.Card, offer.Options = card, options
		offer.ProposalID, err = assistantkit.Offer(ctx, assistantkit.Proposal{Tool: tool, Args: args, Summary: fix.Title, Card: card, Options: options})
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
		args, card, options := slotChoice(c, posts[i], fix)
		if len(args.Moves) == 0 {
			return offer, false, nil
		}
		propose("applyCalendarMoves", args, card, options...)
	case calendarcheck.FixEnablePostType:
		if d.Campaign == nil {
			return offer, false, nil
		}
		propose("updateCampaignPlatforms", campaign_assistant.UpdateCampaignPlatformsInput{
			Platforms: []platforms.Request{{Platform: fix.PlatformID, PostTypes: []string{fix.PostType}}}},
			reAddCard(c, posts, fix))
	case calendarcheck.FixEnablePlatform:
		if d.Campaign == nil {
			return offer, false, nil
		}
		propose("updateCampaignPlatforms", campaign_assistant.UpdateCampaignPlatformsInput{
			Platforms: []platforms.Request{{Platform: fix.PlatformID}}}, nil)
	case calendarcheck.FixConvertPostType:
		propose("changePostType", ChangePostTypeInput{CampaignID: c.ID, PostIDs: fix.PostIDs, PostType: fix.PostType}, nil)
	case calendarcheck.FixUnschedule:
		propose("unschedulePosts", PostsRef{CampaignID: c.ID, PostIDs: fix.PostIDs}, nil)
	case calendarcheck.FixDelete:
		if d.Remove == nil {
			return offer, false, nil
		}
		in := PostsRef{CampaignID: c.ID, PostIDs: fix.PostIDs}
		propose("deletePosts", in, deleteCard(ctx, d, c, posts, in))
	}
	return offer, true, err
}

// slotChoice is the move of one post to the first of its fix's free days,
// and, when there are more, the choice of each day at the campaign's
// publishing time, the first recommended. A day the campaign no longer
// allows is left out.
func slotChoice(c *models.Campaign, p models.Post, fix calendarcheck.Fix) (ApplyMovesInput, *assistantkit.Card, []assistantkit.Option) {
	args := ApplyMovesInput{CampaignID: c.ID}
	var options []assistantkit.Option
	loc := calendarcheck.Location(c)
	days := fix.Slots
	if len(days) == 0 {
		days = []string{fix.To}
	}
	for _, day := range days {
		m := Move{PostID: p.ID, Title: p.Title, From: fix.From, To: day, FromAt: p.ScheduledAt}
		at, ok := moveTarget(c, m)
		if !ok {
			continue
		}
		m.ToAt, m.FreeSlot = &at, true
		description := "Free slot"
		if ph, ok := campaignphase.PhaseAt(c, at); ok && ph.Name != "" {
			description += " · inside the " + ph.Name + " phase"
		}
		if len(args.Moves) == 0 {
			args.Moves = []Move{m}
		}
		options = append(options, assistantkit.Option{ID: day, Title: at.In(loc).Format("Mon 2 Jan, 15:04"), Description: description,
			Recommended: len(options) == 0, Tool: "applyCalendarMoves", Args: ApplyMovesInput{CampaignID: c.ID, Moves: []Move{m}}})
	}
	if len(options) == 0 {
		return args, nil, nil
	}
	card := &assistantkit.Card{Verb: "Move to " + options[0].Title,
		Footnote: "Slots are the campaign's publishing days and times, free of other posts on that channel."}
	if len(options) == 1 {
		return args, card, nil
	}
	card.Meta = plural(len(options), "free slot", "free slots")
	return args, card, options
}

// reAddCard is the card of allowing a dropped post type again: the
// platform with the types it will run, and the posts that fixes.
func reAddCard(c *models.Campaign, posts []models.Post, fix calendarcheck.Fix) *assistantkit.Card {
	name := fix.PlatformID
	if i := slices.IndexFunc(c.Platforms, func(p models.Platform) bool { return p.ID == fix.PlatformID }); i >= 0 {
		name = c.Platforms[i].Name
	}
	var titles []string
	for _, p := range posts {
		if p.PlatformID == fix.PlatformID && p.PlatformPostType == fix.PostType && p.Status != models.PostStatusPublished {
			titles = append(titles, p.Title)
		}
	}
	card := &assistantkit.Card{Verb: "Re-add " + fix.PostType, Meta: "on " + c.Name, Rows: []assistantkit.CardRow{{Kind: assistantkit.CardRowPlatform, Value: name,
		Note: strings.Join(state.AllowedPostTypes(c, fix.PlatformID), ", ") + " + " + fix.PostType}}}
	if len(titles) > 0 {
		fixes := titles[0]
		if len(titles) > 1 {
			fixes += fmt.Sprintf(", and %s", plural(len(titles)-1, "other "+fix.PostType+" post", "other "+fix.PostType+" posts"))
		}
		card.Rows = append(card.Rows, assistantkit.CardRow{Kind: assistantkit.CardRowFixes, Value: fixes})
	}
	return card
}

// deleteCard is the card of deleting posts: each post with its status,
// platform and date, what goes with its media, and that it is final.
func deleteCard(ctx context.Context, d Deps, c *models.Campaign, posts []models.Post, in PostsRef) *assistantkit.Card {
	loc := calendarcheck.Location(c)
	media, known := mediaCounts(ctx, d, in.PostIDs)
	card := &assistantkit.Card{Verb: "Delete " + plural(len(in.PostIDs), "post", "posts"),
		Footnote: "Deleting is final. Nothing in the product brings a post back."}
	if len(in.PostIDs) == 1 {
		card.Verb = "Delete post"
	}
	total := 0
	for _, id := range in.PostIDs {
		i := slices.IndexFunc(posts, func(p models.Post) bool { return p.ID == id })
		if i < 0 {
			continue
		}
		p := posts[i]
		note := []string{strings.ToUpper(string(p.Status[:1])) + strings.ReplaceAll(string(p.Status[1:]), "_", " ")}
		if row := changed(c, p); row.Platform != "" {
			note = append(note, row.Platform)
		}
		if p.ScheduledAt != nil {
			note = append(note, p.ScheduledAt.In(loc).Format("2 Jan"))
		}
		card.Rows = append(card.Rows, assistantkit.CardRow{Kind: assistantkit.CardRowPost, Value: p.Title, Note: strings.Join(note, " · ")})
		total += media[id]
	}
	owner := "Its"
	if len(in.PostIDs) > 1 {
		owner = "Their"
	}
	switch {
	case !known:
		// The files couldn't be counted: say they go, not that there are none.
		card.Rows = append(card.Rows, assistantkit.CardRow{Kind: assistantkit.CardRowMedia, Value: owner + " files, if any",
			Note: "Copies on the post go; the asset in Assets stays"})
	case total > 0:
		card.Rows = append(card.Rows, assistantkit.CardRow{Kind: assistantkit.CardRowMedia, Value: owner + " " + plural(total, "file", "files"),
			Note: "Copies on the post; the asset in Assets stays"})
	}
	return card
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
				"Add a document or import a page first, or go ahead without them."
			p.Options = campaign_assistant.SourceOptions("generatePosts", exploratory)
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
	// ToAt is where it lands, at the campaign's publishing time, for the
	// card's rows; applying works it out again from To.
	ToAt *time.Time `json:"toAt,omitempty"`
	// Status is the post's when offered, so a run can count what it moved.
	Status models.PostStatus `json:"status,omitempty"`
	// FreeSlot marks a move to a day offered as free on the post's
	// platform: it holds only while no other post took that day.
	FreeSlot bool `json:"freeSlot,omitzero"`
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
		m := Move{PostID: p.ID, Title: p.Title, From: from, To: to, FromAt: p.ScheduledAt, Status: p.Status}
		if at, ok := moveTarget(c, m); ok {
			m.ToAt = &at
		}
		out = append(out, m)
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
	CampaignID string `json:"campaignId"`
	ProposalID string `json:"proposalId,omitempty"`
	// Window is the dates the moves spread over: the campaign's, as the
	// redistribution plans over all of them.
	Window    Window       `json:"window"`
	Moves     []Move       `json:"moves"`
	Locked    []LockedPost `json:"locked"`
	Note      string       `json:"note,omitempty"`
	CheckedAt time.Time    `json:"checkedAt"`
}

// Window is a span of days, YYYY-MM-DD, both included.
type Window struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// LockedPost is a scheduled post a redistribution doesn't move: a copy of it
// waits to publish.
type LockedPost struct {
	PostID string `json:"postId"`
	Title  string `json:"title,omitempty"`
	// Date is its day in the campaign's timezone, YYYY-MM-DD; At its exact
	// time.
	Date string    `json:"date"`
	At   time.Time `json:"at"`
}

// locked are the campaign's scheduled posts, by date.
func locked(c *models.Campaign, posts []models.Post) []LockedPost {
	loc := calendarcheck.Location(c)
	out := []LockedPost{}
	for _, p := range posts {
		if p.ScheduledAt == nil || reschedule.Eligible(p.Status) || p.Status == models.PostStatusPublished {
			continue
		}
		out = append(out, LockedPost{PostID: p.ID, Title: p.Title, Date: p.ScheduledAt.In(loc).Format(time.DateOnly), At: *p.ScheduledAt})
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
	now := time.Now().UTC()
	out := &OptimiseOutput{CampaignID: c.ID, Moves: moves(c, posts), Locked: locked(c, posts), CheckedAt: now,
		Window: Window{From: campaignphase.Date(*c.StartDate).Format(time.DateOnly), To: campaignphase.Date(*c.EndDate).Format(time.DateOnly)}}
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
	// Moves are the moves made, each from its old time to its new one.
	Moves []Move `json:"moves"`
	// ByStatus counts the moves by the posts' status, so moving a post
	// ready to publish is said, not a surprise.
	ByStatus map[models.PostStatus]int `json:"byStatus"`
	// Locked counts the scheduled posts left where they were.
	Locked int    `json:"locked"`
	UndoID string `json:"undoId,omitempty"`
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
	out := &ApplyMovesOutput{Moves: []Move{}, ByStatus: map[models.PostStatus]int{}, Locked: len(locked(c, posts))}
	var batch []repository.ScheduleMove
	made := map[string]Move{}
	claimed := map[string]bool{}
	for _, m := range in.Moves {
		p := byID[m.PostID]
		at, ok := moveHolds(c, p, m)
		if !ok || slotTaken(c, posts, p, m, claimed) {
			out.Skipped = append(out.Skipped, m.PostID)
			continue
		}
		batch = append(batch, repository.ScheduleMove{PostID: m.PostID, From: m.FromAt, To: at})
		m.Title, m.ToAt, m.Status = p.Title, &at, p.Status
		made[m.PostID] = m
	}
	if len(batch) == 0 {
		return out, nil
	}
	// The write re-checks each post's schedule, for an edit racing this one.
	moved, err := d.Posts.MoveScheduledAt(ctx, batch, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("move posts: %w", err)
	}
	var before, after []assistantundo.PostState
	for _, m := range batch {
		if !slices.Contains(moved, m.PostID) {
			out.Skipped = append(out.Skipped, m.PostID)
			continue
		}
		mv := made[m.PostID]
		out.Moves = append(out.Moves, mv)
		out.ByStatus[mv.Status]++
		before = append(before, assistantundo.PostState{ID: m.PostID, ScheduledAt: m.From})
		after = append(after, assistantundo.PostState{ID: m.PostID, ScheduledAt: new(m.To)})
	}
	if len(moved) > 0 {
		out.UndoID = recordUndo(ctx, d, models.AssistantActionPostSchedules, c.ID, assistantundo.Posts(before), assistantundo.Posts(after))
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

// slotTaken reports whether a free-slot move's day has another post on the
// post's platform now, or was claimed by an earlier move of the batch, which
// it then claims. A move that isn't to a free slot is never taken.
func slotTaken(c *models.Campaign, posts []models.Post, p *models.Post, m Move, claimed map[string]bool) bool {
	if !m.FreeSlot || p == nil {
		return false
	}
	key := p.PlatformID + "|" + m.To
	if claimed[key] {
		return true
	}
	loc := calendarcheck.Location(c)
	if slices.ContainsFunc(posts, func(o models.Post) bool {
		return o.ID != p.ID && o.PlatformID == p.PlatformID && o.ScheduledAt != nil && o.ScheduledAt.In(loc).Format(time.DateOnly) == m.To
	}) {
		return true
	}
	claimed[key] = true
	return false
}

// movesThatHold narrows confirmed moves to the ones that still hold, saying
// why each other one doesn't; none is a refusal.
func movesThatHold(ctx context.Context, d Deps, in ApplyMovesInput) (ApplyMovesInput, []string, error) {
	c, err := d.loadCampaign(ctx, CampaignRef{CampaignID: in.CampaignID})
	if err != nil {
		return in, nil, err
	}
	posts, err := d.Posts.ListByCampaign(ctx, c.ID)
	if err != nil {
		return in, nil, err
	}
	var changes []string
	deleted := 0
	claimed := map[string]bool{}
	keep := slices.DeleteFunc(slices.Clone(in.Moves), func(m Move) bool {
		i := slices.IndexFunc(posts, func(p models.Post) bool { return p.ID == m.PostID })
		if i < 0 {
			deleted++
			return true
		}
		if _, ok := moveHolds(c, &posts[i], m); !ok {
			changes = append(changes, whyMoveFails(c, &posts[i], m))
			return true
		}
		if slotTaken(c, posts, &posts[i], m, claimed) {
			changes = append(changes, whySlotTaken(&posts[i], m))
			return true
		}
		return false
	})
	switch {
	case deleted == 1:
		changes = append(changes, "A post the offer moved was deleted")
	case deleted > 1:
		changes = append(changes, fmt.Sprintf("%d posts the offer moved were deleted", deleted))
	}
	if len(keep) == 0 {
		return in, changes, assistantkit.Refused("none of those moves hold any more: the posts were moved, scheduled or deleted since")
	}
	if len(keep) < len(in.Moves) {
		in.Moves = keep
	}
	return in, changes, nil
}

// whyMoveFails says why m no longer holds for p, which is still there.
func whyMoveFails(c *models.Campaign, p *models.Post, m Move) string {
	title := "A post"
	if p.Title != "" {
		title = "“" + p.Title + "”"
	}
	switch {
	case p.Status == models.PostStatusPublished:
		return title + " was published since"
	case !reschedule.Eligible(p.Status):
		return title + " was scheduled since — scheduled posts stay put"
	case !sameInstant(p.ScheduledAt, m.FromAt):
		return title + " was moved since"
	}
	return title + " can't go to " + m.To + " any more: the campaign doesn't publish then"
}

// whySlotTaken says that a free slot a move chose has a post on it now.
func whySlotTaken(p *models.Post, m Move) string {
	title := "A post"
	if p.Title != "" {
		title = "“" + p.Title + "”"
	}
	return title + " can't go to " + m.To + " any more: another post on its platform is there now"
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

// RestoreFindingOutput says whether the finding was dismissed and is back.
type RestoreFindingOutput struct {
	Restored bool `json:"restored"`
}

// restoreFinding takes back the campaign's dismissal of the finding, and
// this thread's from before dismissals followed the campaign.
func restoreFinding(ctx context.Context, d Deps, in DismissFindingInput) (*RestoreFindingOutput, error) {
	if d.Dismissals == nil {
		return nil, assistantkit.Refused("findings can't be restored here")
	}
	if strings.TrimSpace(in.Key) == "" {
		return nil, assistantkit.Refused("pass the finding's key")
	}
	c, err := d.loadCampaign(ctx, CampaignRef{CampaignID: in.CampaignID})
	if err != nil {
		return nil, err
	}
	restored, err := d.Dismissals.Restore(ctx, models.AssistantScopeCampaign, c.ID, in.Key)
	if err != nil {
		return nil, err
	}
	if scope, ok := assistant.Scope(ctx); ok && (scope.Kind != models.AssistantScopeCampaign || scope.ID != c.ID) {
		inThread, err := d.Dismissals.Restore(ctx, scope.Kind, scope.ID, in.Key)
		if err != nil {
			return nil, err
		}
		restored = restored || inThread
	}
	return &RestoreFindingOutput{Restored: restored}, nil
}

// ReviewFinding is a brief or post finding with its fixes.
type ReviewFinding = campaign_assistant.ReviewFinding

// ReviewOutput is the campaign's review.
type ReviewOutput = campaign_assistant.ReviewOutput

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
	dismissed, err := campaign_assistant.DismissedKeys(ctx, d.Dismissals, c.ID)
	if err != nil {
		return nil, err
	}
	out := campaign_assistant.NewReview(c.ID)
	can := campaign_assistant.ReviewOffers{Brief: d.Campaign != nil, Revise: d.Revise != nil}
	if err := out.AddBrief(ctx, brief, dismissed, can); err != nil {
		return nil, err
	}
	if err := out.AddPosts(ctx, postsReview, dismissed, can); err != nil {
		return nil, err
	}
	return out, nil
}

// RevisePostInput is a post of the campaign and how to rewrite it.
type RevisePostInput = campaign_assistant.RevisePostInput

// RevisePostOutput confirms the rewrite, and whether it was saved as a
// version.
type RevisePostOutput struct {
	PostID string `json:"postId"`
	// CampaignID is the post's campaign, which opening it needs.
	CampaignID string `json:"campaignId"`
	Title      string `json:"title,omitempty"`
	// VoiceName is the voice the rewrite was written in.
	VoiceName string `json:"voiceName,omitempty"`
	// UndoID puts the copy it replaced back.
	UndoID    string `json:"undoId,omitempty"`
	Revised   bool   `json:"revised"`
	Versioned bool   `json:"versioned"`
	Note      string `json:"note"`
	// Grounding is the post's own: a rewrite keeps its sources. Unset when
	// the post's never was known.
	Grounding *assistantkit.Grounding `json:"grounding,omitempty"`
	// Length is the copy's length in characters before and after; unset
	// when the rewrite couldn't be read back.
	Length *Lengths `json:"length,omitempty"`
}

// Lengths is a text's length in characters before and after a change.
type Lengths struct {
	Before int `json:"before"`
	After  int `json:"after"`
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
	was := posts[0]
	rev, err := d.Revise(ctx, in.PostID, in.Instruction, "Rewritten by the campaign's assistant")
	if err != nil {
		return nil, fmt.Errorf("rewrite post: %w", err)
	}
	out := &RevisePostOutput{PostID: in.PostID, CampaignID: c.ID, Title: was.Title, Revised: true, Versioned: rev.Versioned,
		Note:      "saved as a new version",
		Grounding: postGrounding(ctx, d, c.ID, was)}
	if !rev.Versioned {
		out.Note = "the post is rewritten, but saving the rewrite as a version failed, so its history has no copy of it"
	}
	// The undo holds what this rewrite wrote, so it refuses once anyone
	// edits the post after it.
	written := was
	written.Content, written.Status = rev.Content, rev.Status
	out.UndoID = recordUndo(ctx, d, models.AssistantActionPostRewritten, in.PostID, assistantundo.PostContent(&was), assistantundo.PostContent(&written))
	if out.UndoID != "" {
		out.Note += "; it can be undone"
	}
	if d.Brand != nil {
		if r, err := brandresolve.Resolve(ctx, d.Brand, c, &was); err == nil && r.Voice != nil {
			out.VoiceName = r.Voice.Name
		}
	}
	out.Length = &Lengths{Before: utf8.RuneCountInString(was.Content), After: utf8.RuneCountInString(rev.Content)}
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
