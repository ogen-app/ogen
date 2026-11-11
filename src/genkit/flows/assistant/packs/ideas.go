package packs

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
)

const ideasPackName = "ideas"

// maxIdeaIDs caps one bulk call; a larger set is a filter for the list.
const maxIdeaIDs = viewcontext.MaxSelectionIDs

// ideasPack finds, triages, moves and deletes ideas, and turns them into
// posts: a planned post with their theses, or written drafts.
func ideasPack(d Deps) assistant.Pack {
	return assistant.Pack{
		Name:         ideasPackName,
		Summary:      "find ideas, triage them (yes, later, no), move or delete them, and compile or draft them into posts",
		Screens:      []viewcontext.Screen{"ideas", "campaign.ideas"},
		Instructions: ideasRules,
		Begin:        ideasBegin(d),
		End:          endResults(ideasPackName),
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			find := genkit.DefineTool(g, "findIdeas",
				"Lists ideas matching a filter in the Ideas list's syntax, with their id, title, verdict, origin and plan.",
				assistantkit.Wrap(recorded(ideasPackName, "findIdeas", nil, func(ctx context.Context, in FindIdeasInput) (*FindIdeasOutput, error) {
					return findIdeas(ctx, d, in)
				})), ai.WithStrictSchema(false))
			verdict := genkit.DefineTool(g, "setIdeaVerdict",
				"Triages one idea: yes, later (with a reminder date), no, or none to put it back in the inbox. Returns an undo id.",
				onScreen(func(ctx context.Context, in *SetIdeaVerdictInput) { in.IdeaID = ideaOnScreen(ctx, in.IdeaID) },
					assistantkit.Wrap(recorded(ideasPackName, "setIdeaVerdict", nil, func(ctx context.Context, in SetIdeaVerdictInput) (*SetIdeaVerdictOutput, error) {
						return setIdeaVerdict(ctx, d, in)
					}))), ai.WithStrictSchema(false))
			verdicts := genkit.DefineTool(g, "setIdeaVerdicts",
				"Triages several ideas at once with the same verdict. Above a few ideas it is offered for the user to confirm first.",
				onScreen(func(ctx context.Context, in *SetIdeaVerdictsInput) { in.IdeaIDs = ideasOnScreen(ctx, in.IdeaIDs) },
					assistantkit.GatedRetry("setIdeaVerdicts", assistantkit.Gate[SetIdeaVerdictsInput]{
						Precheck: func(_ context.Context, in SetIdeaVerdictsInput) error { return checkIDs(in.IdeaIDs) },
						Recheck: recheckIDs(d, viewcontext.KindIdea, func(_ context.Context, in SetIdeaVerdictsInput) (string, []string) { return ideasIn(in.IdeaIDs) },
							func(in SetIdeaVerdictsInput, ids []string) SetIdeaVerdictsInput { in.IdeaIDs = ids; return in }),
						Needs: func(_ context.Context, in SetIdeaVerdictsInput) bool { return overN(d, len(in.IdeaIDs)) },
						Summary: func(in SetIdeaVerdictsInput) string {
							return fmt.Sprintf("Mark %s %s", plural(len(in.IdeaIDs), "idea", "ideas"), verdictLabel(in.Verdict))
						},
					}, ideaRetry(func(in SetIdeaVerdictsInput, ids []string) SetIdeaVerdictsInput { in.IdeaIDs = ids; return in }), assistantkit.Wrap(recorded(ideasPackName, "setIdeaVerdicts", nil, func(ctx context.Context, in SetIdeaVerdictsInput) (*BulkIdeasOutput, error) {
						return setIdeaVerdicts(ctx, d, in)
					})))), ai.WithStrictSchema(false))
			move := genkit.DefineTool(g, "moveIdeas",
				"Moves ideas into a campaign, or out to the workspace backlog. Always offered for the user to confirm first.",
				onScreen(func(ctx context.Context, in *MoveIdeasInput) { in.IdeaIDs = ideasOnScreen(ctx, in.IdeaIDs) },
					assistantkit.GatedRetry("moveIdeas", assistantkit.Gate[MoveIdeasInput]{
						Precheck: func(_ context.Context, in MoveIdeasInput) error { return checkIDs(in.IdeaIDs) },
						Recheck: recheckIDs(d, viewcontext.KindIdea, func(_ context.Context, in MoveIdeasInput) (string, []string) { return ideasIn(in.IdeaIDs) },
							func(in MoveIdeasInput, ids []string) MoveIdeasInput { in.IdeaIDs = ids; return in }),
						Summary: func(in MoveIdeasInput) string {
							if in.CampaignID == "" {
								return fmt.Sprintf("Move %s to the workspace backlog", plural(len(in.IdeaIDs), "idea", "ideas"))
							}
							return fmt.Sprintf("Move %s into the campaign", plural(len(in.IdeaIDs), "idea", "ideas"))
						},
					}, ideaRetry(func(in MoveIdeasInput, ids []string) MoveIdeasInput { in.IdeaIDs = ids; return in }), assistantkit.Wrap(recorded(ideasPackName, "moveIdeas", nil, func(ctx context.Context, in MoveIdeasInput) (*BulkIdeasOutput, error) {
						return moveIdeas(ctx, d, in)
					})))), ai.WithStrictSchema(false))
			del := genkit.DefineTool(g, "deleteIdeas",
				"Deletes ideas for good. Always offered for the user to confirm first.",
				onScreen(func(ctx context.Context, in *DeleteIdeasInput) { in.IdeaIDs = ideasOnScreen(ctx, in.IdeaIDs) },
					assistantkit.Gated("deleteIdeas", assistantkit.Gate[DeleteIdeasInput]{
						Precheck: func(_ context.Context, in DeleteIdeasInput) error { return checkIDs(in.IdeaIDs) },
						Recheck: recheckIDs(d, viewcontext.KindIdea, func(_ context.Context, in DeleteIdeasInput) (string, []string) { return ideasIn(in.IdeaIDs) },
							func(in DeleteIdeasInput, ids []string) DeleteIdeasInput { in.IdeaIDs = ids; return in }),
						Summary: func(in DeleteIdeasInput) string {
							return fmt.Sprintf("Delete %s", plural(len(in.IdeaIDs), "idea", "ideas"))
						},
					}, assistantkit.Wrap(recorded(ideasPackName, "deleteIdeas", nil, func(ctx context.Context, in DeleteIdeasInput) (*BulkIdeasOutput, error) {
						return deleteIdeas(ctx, d, in)
					})))), ai.WithStrictSchema(false))
			compile := genkit.DefineTool(g, "compileIdeas",
				"Compiles one or more ideas into a planned post on each platform: their titles and angles become its theses, their plan suggests its platform, type, phase and date, and the post links back to them. No body is written; without a date the posts stay undated. Returns an undo id.",
				onScreen(func(ctx context.Context, in *CompileIdeasInput) { in.IdeaIDs = ideasOnScreen(ctx, in.IdeaIDs) },
					assistantkit.Wrap(recorded(ideasPackName, "compileIdeas", nil, func(ctx context.Context, in CompileIdeasInput) (*CompileIdeasOutput, error) {
						return compileIdeas(ctx, d, in)
					}))), ai.WithStrictSchema(false))
			draft := genkit.DefineTool(g, "draftIdeas",
				"Writes finished posts from ideas and saves them as drafts with a body: one per idea per platform, or with together one per platform from all of them. Without a date, given or planned on the ideas, the drafts are unscheduled. Above a few posts it is offered for the user to confirm first.",
				onScreen(func(ctx context.Context, in *DraftIdeasInput) { in.IdeaIDs = ideasOnScreen(ctx, in.IdeaIDs) },
					assistantkit.GatedRetry("draftIdeas", assistantkit.Gate[DraftIdeasInput]{
						Precheck: func(ctx context.Context, in DraftIdeasInput) error {
							_, err := planDraft(ctx, d, in)
							return err
						},
						Recheck: recheckIDs(d, viewcontext.KindIdea, func(_ context.Context, in DraftIdeasInput) (string, []string) { return ideasIn(in.IdeaIDs) },
							func(in DraftIdeasInput, ids []string) DraftIdeasInput { in.IdeaIDs = ids; return in }),
						Needs: func(ctx context.Context, in DraftIdeasInput) bool {
							plan, err := planDraft(ctx, d, in)
							return err == nil && overN(d, len(plan.Jobs))
						},
						Summary: func(in DraftIdeasInput) string {
							return fmt.Sprintf("Write posts from %s", plural(len(in.IdeaIDs), "idea", "ideas"))
						},
					}, draftRetry, assistantkit.Wrap(recorded(ideasPackName, "draftIdeas", nil, func(ctx context.Context, in DraftIdeasInput) (*DraftIdeasOutput, error) {
						return draftIdeas(ctx, d, in)
					})))), ai.WithStrictSchema(false))

			return append([]assistantkit.Spec{
				spec(ideasPackName, find, assistantkit.TierRead,
					`Call this to look up ideas by what they are about or by state — e.g. "which ideas are about pricing?", "the undecided ones from analyses". filter is a URL query string in the Ideas list's syntax: text=, pile=waiting|yes|later|no, origin=asset_analysis|content_plan|assistant|manual, fit=high|medium|low, campaign_id=<id>, used=unused|used (whether a post was made from it — "which ideas haven't been made into posts yet?" is used=unused). To show the user, also call setFilter with the same filter.`),
				spec(ideasPackName, verdict, assistantkit.TierReversible,
					`Call this to triage ONE idea — "yes to this one", "snooze it until Monday", "not this". ideaId defaults to the idea open on screen, else the one selected. later needs remindAt (YYYY-MM-DD or RFC 3339). It can be undone.`),
				spec(ideasPackName, verdicts, assistantkit.TierProposal,
					`Call this to triage SEVERAL ideas the same way — "mark all of these yes". ideaIds default to the ideas selected on screen; otherwise pass them (from getSelection or findIdeas, at most 200). More than a few are offered first; relay that. It can be undone.`),
				spec(ideasPackName, move, assistantkit.TierProposal,
					`Call this to move ideas into a campaign (campaignId) or back to the workspace backlog (campaignId empty). ideaIds default to the selected ideas, else the open one. Always offered first.`),
				spec(ideasPackName, del, assistantkit.TierProposal,
					`Call this when the user asks to delete ideas. ideaIds default to the selected ideas, else the open one. Always offered first; it can't be undone.`),
				spec(ideasPackName, compile, assistantkit.TierReversible,
					`Call this to turn ideas into ONE planned post — "make a post from these three", "combine these", "plan a post from these for LinkedIn and X". It writes no body: the post holds their theses until someone drafts it. Several platforms get one planned post each. ideaIds default to the selected ideas, else the open one; pass them in the order the theses should come. The campaign defaults to the one they share. Pass targetDate only when the user names a date; otherwise the posts stay off the calendar, so say they're undated. It can be undone while no one has changed the posts.`),
				spec(ideasPackName, draft, assistantkit.TierProposal,
					`Call this to write finished posts from ideas — "draft LinkedIn and Instagram versions of these". ideaIds default to the selected ideas, else the open one. platformIds default to the ideas' suggested platform; together writes one post per platform from all of them. Pass targetDate only when the user names a date; without one (or a date planned on the ideas) the drafts are unscheduled — in Drafts, not on the calendar — so say that, and that they can be dated later. More than a few posts are offered first.`),
			}, slices.Concat(defineReadTools(g, d), defineEditTools(g, d), defineMergeTools(g, d))...), nil
		},
	}
}

func checkIDs(ids []string) error {
	n := len(dedupe(ids))
	if n == 0 {
		return assistantkit.Refused("pass the ideas' ids; get them from getSelection or findIdeas")
	}
	if n > maxIdeaIDs {
		return assistantkit.Refused(fmt.Sprintf("at most %d ideas at once; narrow the selection", maxIdeaIDs))
	}
	return nil
}

func dedupe(ids []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// FindIdeasInput is an Ideas list query.
type FindIdeasInput struct {
	Filter string `json:"filter,omitempty" jsonschema:"description=A URL query string in the Ideas list's syntax, e.g. text=pricing&pile=waiting"`
	Limit  int    `json:"limit,omitempty"  jsonschema:"description=How many ideas to return; at most 50"`
}

// IdeaRef is an idea as the assistant sees it.
type IdeaRef struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Verdict    string          `json:"verdict,omitempty"`
	Origin     string          `json:"origin,omitempty"`
	CampaignID string          `json:"campaignId,omitempty"`
	FitScore   *int            `json:"fitScore,omitempty"`
	Plan       models.IdeaPlan `json:"plan"`
	// Posts counts the posts made from the idea.
	Posts int `json:"posts,omitempty"`
}

// FindIdeasOutput is one page of matches.
type FindIdeasOutput struct {
	Ideas []IdeaRef `json:"ideas"`
	Total int       `json:"total"`
}

func findIdeas(ctx context.Context, d Deps, in FindIdeasInput) (*FindIdeasOutput, error) {
	v, err := url.ParseQuery(in.Filter)
	if err != nil {
		return nil, assistantkit.Refused("the filter is not a query string")
	}
	q, err := ideas.ParseQuery(v, time.Now())
	if err != nil {
		return nil, assistantkit.Refused("the Ideas list rejects that filter: " + err.Error())
	}
	q.ScopeCampaign = assistant.CampaignID(ctx)
	q.Limit = 20
	if in.Limit > 0 {
		q.Limit = min(in.Limit, 50)
	}
	res, err := d.Ideas.Query(ctx, q)
	if errors.Is(err, ideas.ErrScopeNotFound) {
		return nil, assistantkit.Refused("this thread's campaign is gone")
	}
	if err != nil {
		return nil, err
	}
	out := &FindIdeasOutput{Ideas: make([]IdeaRef, len(res.Ideas)), Total: res.Total}
	for i, idea := range res.Ideas {
		out.Ideas[i] = ideaRef(idea)
	}
	return out, nil
}

func ideaRef(i models.Idea) IdeaRef {
	r := IdeaRef{ID: i.ID, Title: i.Title, Origin: string(i.Origin), FitScore: i.FitScore, Plan: i.Plan, Posts: i.PostCount}
	if i.Verdict != nil {
		r.Verdict = string(*i.Verdict)
	}
	if i.CampaignID != nil {
		r.CampaignID = *i.CampaignID
	}
	return r
}

// SetIdeaVerdictInput triages one idea.
type SetIdeaVerdictInput struct {
	IdeaID   string `json:"ideaId,omitempty"   jsonschema:"description=Defaults to the idea open on screen, else the one selected"`
	Verdict  string `json:"verdict"            jsonschema:"description=yes, later, no, or none to put it back in the inbox"`
	RemindAt string `json:"remindAt,omitempty" jsonschema:"description=When a later idea comes back: YYYY-MM-DD or RFC 3339"`
}

// SetIdeaVerdictOutput is the triaged idea and the undo handle.
type SetIdeaVerdictOutput struct {
	Idea   IdeaRef `json:"idea"`
	UndoID string  `json:"undoId,omitempty"`
}

func parseVerdict(verdict, remindAt string) (*models.IdeaVerdict, *time.Time, error) {
	var v *models.IdeaVerdict
	switch strings.ToLower(strings.TrimSpace(verdict)) {
	case "", "none", "inbox", "null":
	case "yes":
		v = new(models.IdeaVerdictYes)
	case "later":
		v = new(models.IdeaVerdictLater)
	case "no":
		v = new(models.IdeaVerdictNo)
	default:
		return nil, nil, assistantkit.Refused("verdict is yes, later, no, or none", "yes", "later", "no", "none")
	}
	if remindAt == "" {
		return v, nil, nil
	}
	if t, err := time.Parse(time.RFC3339, remindAt); err == nil {
		return v, &t, nil
	}
	t, err := time.Parse(time.DateOnly, remindAt)
	if err != nil {
		return nil, nil, assistantkit.Refused("remindAt is a YYYY-MM-DD date or an RFC 3339 time")
	}
	return v, &t, nil
}

func verdictLabel(v string) string {
	switch strings.ToLower(v) {
	case "yes", "later", "no":
		return strings.ToLower(v)
	}
	return "back to the inbox"
}

// triageErr turns the ideas service's input errors into refusals.
func triageErr(err error) error {
	if errors.Is(err, ideas.ErrNotFound) {
		return assistantkit.Refused("that idea doesn't exist")
	}
	if ideas.IsValidation(err) {
		return assistantkit.Refused(err.Error())
	}
	return err
}

func setIdeaVerdict(ctx context.Context, d Deps, in SetIdeaVerdictInput) (*SetIdeaVerdictOutput, error) {
	if strings.TrimSpace(in.IdeaID) == "" {
		return nil, assistantkit.Refused("which idea? open it, select it, or pass ideaId")
	}
	verdict, remindAt, err := parseVerdict(in.Verdict, in.RemindAt)
	if err != nil {
		return nil, err
	}
	before, err := d.Ideas.Get(ctx, in.IdeaID)
	if err != nil {
		return nil, triageErr(err)
	}
	snapshot := assistantundo.IdeaVerdict(before)
	idea, _, err := d.Ideas.SetVerdict(ctx, in.IdeaID, verdict, remindAt, actor(ctx))
	if err != nil {
		return nil, triageErr(err)
	}
	assistantkit.Touch(ctx, "idea", idea.ID)
	undo := recordUndo(ctx, d, models.AssistantActionIdeaVerdict, idea.ID, snapshot, assistantundo.IdeaVerdict(idea))
	return &SetIdeaVerdictOutput{Idea: ideaRef(*idea), UndoID: undo}, nil
}

// SetIdeaVerdictsInput triages several ideas the same way.
type SetIdeaVerdictsInput struct {
	IdeaIDs  []string `json:"ideaIds,omitempty"  jsonschema:"description=Default the ideas selected on screen, else the open one"`
	Verdict  string   `json:"verdict"            jsonschema:"description=yes, later, no, or none"`
	RemindAt string   `json:"remindAt,omitempty" jsonschema:"description=For later: YYYY-MM-DD or RFC 3339"`
}

// BulkIdeasOutput is what a bulk change did.
type BulkIdeasOutput struct {
	Done   int      `json:"done"`
	Failed []string `json:"failed,omitempty"`
	// Stopped is why the change stopped part way; the ideas after it were
	// not changed.
	Stopped string `json:"stopped,omitempty"`
	// UndoID undoes a triage, the ideas it changed all at once.
	UndoID string `json:"undoId,omitempty"`
	// left is the ideas a stop on an error left unchanged, worth a retry.
	left []string
	assistantkit.Partial
}

// stoppedEarly ends a bulk change that failed part way on left, the ideas
// from the one it failed on. What it changed before stays changed, so it is
// reported and touched for the client to refresh rather than lost in the
// error; with nothing changed the error stands. Ideas left by an error are
// worth a retry; by a refusal, not.
func stoppedEarly(ctx context.Context, out *BulkIdeasOutput, left []string, err error) (*BulkIdeasOutput, error) {
	if out.Done == 0 {
		return out, err
	}
	assistantkit.Touch(ctx, "idea", "")
	reason := err.Error()
	if r, ok := errors.AsType[*assistantkit.Refusal](err); ok {
		reason = r.Reason
	} else {
		out.left = left
	}
	out.Stopped = "stopped early: " + reason
	return out, nil
}

func setIdeaVerdicts(ctx context.Context, d Deps, in SetIdeaVerdictsInput) (*BulkIdeasOutput, error) {
	verdict, remindAt, err := parseVerdict(in.Verdict, in.RemindAt)
	if err != nil {
		return nil, err
	}
	out := &BulkIdeasOutput{}
	var before, after []*models.Idea
	// What changed is logged for undo whether or not the run finished.
	defer func() {
		if len(after) > 0 {
			out.UndoID = recordUndo(ctx, d, models.AssistantActionIdeaVerdicts, "", assistantundo.IdeaVerdicts(before), assistantundo.IdeaVerdicts(after))
		}
	}()
	ids := dedupe(in.IdeaIDs)
	for i, id := range ids {
		was, err := d.Ideas.Get(ctx, id)
		if err == nil {
			var idea *models.Idea
			if idea, _, err = d.Ideas.SetVerdict(ctx, id, verdict, remindAt, actor(ctx)); err == nil {
				before, after = append(before, was), append(after, idea)
				out.Done++
				continue
			}
		}
		if !errors.Is(err, ideas.ErrNotFound) && !ideas.IsValidation(err) {
			return stoppedEarly(ctx, out, ids[i:], err)
		}
		out.Failed = append(out.Failed, id)
	}
	assistantkit.Touch(ctx, "idea", "")
	return out, nil
}

// MoveIdeasInput moves ideas into a campaign or out of one.
type MoveIdeasInput struct {
	IdeaIDs    []string `json:"ideaIds,omitempty"    jsonschema:"description=Default the ideas selected on screen, else the open one"`
	CampaignID string   `json:"campaignId,omitempty" jsonschema:"description=The campaign to move them into; empty moves them to the workspace backlog"`
}

func moveIdeas(ctx context.Context, d Deps, in MoveIdeasInput) (*BulkIdeasOutput, error) {
	var campaign *string
	if id := strings.TrimSpace(in.CampaignID); id != "" {
		campaign = &id
	}
	out := &BulkIdeasOutput{}
	ids := dedupe(in.IdeaIDs)
	for i, id := range ids {
		if _, _, err := d.Ideas.Update(ctx, id, ideas.UpdateInput{SetCampaign: true, CampaignID: campaign}); err != nil {
			if errors.Is(err, ideas.ErrCampaignNotFound) {
				return stoppedEarly(ctx, out, ids[i:], assistantkit.Refused("that campaign doesn't exist"))
			}
			if !errors.Is(err, ideas.ErrNotFound) {
				return stoppedEarly(ctx, out, ids[i:], err)
			}
			out.Failed = append(out.Failed, id)
			continue
		}
		out.Done++
	}
	assistantkit.Touch(ctx, "idea", "")
	return out, nil
}

// DeleteIdeasInput deletes ideas.
type DeleteIdeasInput struct {
	IdeaIDs []string `json:"ideaIds,omitempty" jsonschema:"description=Default the ideas selected on screen, else the open one"`
}

func deleteIdeas(ctx context.Context, d Deps, in DeleteIdeasInput) (*BulkIdeasOutput, error) {
	deleted, failed, err := d.Ideas.BulkDelete(ctx, dedupe(in.IdeaIDs))
	if err != nil {
		return nil, triageErr(err)
	}
	out := &BulkIdeasOutput{Done: len(deleted)}
	for _, f := range failed {
		out.Failed = append(out.Failed, f.ID)
	}
	assistantkit.Touch(ctx, "idea", "")
	return out, nil
}

// CompileIdeasInput turns ideas into a planned post per platform.
type CompileIdeasInput struct {
	IdeaIDs     []string `json:"ideaIds,omitempty"     jsonschema:"description=The ideas, in the order their theses should come; default the selected ideas, else the open one"`
	Title       string   `json:"title,omitempty"       jsonschema:"description=The posts' title; defaults to the first idea's"`
	CampaignID  string   `json:"campaignId,omitempty"  jsonschema:"description=The posts' campaign; defaults to the one the ideas share"`
	PlatformIDs []string `json:"platformIds,omitempty" jsonschema:"description=One post per platform; default the ideas' suggested platform"`
	PostType    string   `json:"postType,omitempty"    jsonschema:"description=Needs exactly one platform"`
	TargetDate  string   `json:"targetDate,omitempty"  jsonschema:"description=YYYY-MM-DD; only when the user names a date or the ideas plan one, else the posts stay undated"`
}

// CompileIdeasOutput is the planned posts: the first one's fields, then
// every post, one per platform.
type CompileIdeasOutput struct {
	CompiledPost
	Posts      []CompiledPost `json:"posts"`
	CampaignID string         `json:"campaignId"`
	Ideas      int            `json:"ideas"`
	// Grounding is the ideas' assets the posts' theses rest on; exploratory
	// when they had none.
	Grounding assistantkit.Grounding `json:"grounding"`
	// UndoID deletes the posts again while no one has changed them.
	UndoID string `json:"undoId,omitempty"`
}

// CompiledPost is one planned post. Date is empty for an undated one.
type CompiledPost struct {
	PostID     string `json:"postId"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	PlatformID string `json:"platformId,omitempty"`
	Date       string `json:"date,omitempty"`
}

func calendarDate(s string) (*models.CalendarDate, error) {
	if s == "" {
		return nil, nil
	}
	d, err := models.ParseCalendarDate(s)
	if err != nil {
		return nil, assistantkit.Refused("dates are YYYY-MM-DD")
	}
	return &d, nil
}

func composeErr(err error) error {
	if compose.IsValidation(err) {
		return assistantkit.Refused(err.Error())
	}
	return err
}

func compileIdeas(ctx context.Context, d Deps, in CompileIdeasInput) (*CompileIdeasOutput, error) {
	date, err := calendarDate(in.TargetDate)
	if err != nil {
		return nil, err
	}
	compile := func(campaignID string) (*compose.Result, error) {
		return d.Compose.Compile(ctx, compose.Input{
			IdeaIDs: in.IdeaIDs, CampaignID: campaignID, Title: in.Title,
			PlatformIDs: in.PlatformIDs, PostType: in.PostType, TargetDate: date,
			Actor: actor(ctx), NoteOrigin: models.PostNoteOriginAssistant,
		})
	}
	res, err := compile(in.CampaignID)
	if thread := assistant.CampaignID(ctx); in.CampaignID == "" && thread != "" && orphaned(err) {
		res, err = compile(thread)
	}
	if err != nil {
		return nil, composeErr(err)
	}
	first := res.Posts[0].Post
	out := &CompileIdeasOutput{CampaignID: first.CampaignID, Ideas: len(res.Ideas),
		Grounding: analysed(ctx, d, &first.CampaignID, newAssetTitles(d).grounding(ctx, fromIdeas(first.UsedAssetIDs, res.Ideas)))}
	made := make([]assistantundo.MadePost, len(res.Posts))
	for i, c := range res.Posts {
		p := c.Post
		assistantkit.Touch(ctx, "post", p.ID)
		made[i] = assistantundo.MadePost{Post: p, Notes: []models.PostNote{*c.Note}}
		cp := CompiledPost{PostID: p.ID, Title: p.Title, Status: string(p.Status), PlatformID: p.PlatformID}
		if p.ScheduledAt != nil {
			cp.Date = p.ScheduledAt.Format(time.DateOnly)
		}
		out.Posts = append(out.Posts, cp)
	}
	out.CompiledPost = out.Posts[0]
	out.CompiledPost = out.Posts[0]
	out.UndoID = recordUndo(ctx, d, models.AssistantActionPostCreated, first.ID, models.JSONMap{}, assistantundo.PostsCreated(made))
	return out, nil
}

// assetTitles names assets for grounding, reading each once.
type assetTitles struct {
	assets repository.AssetRepository
	known  map[string]string
}

func newAssetTitles(d Deps) *assetTitles {
	return &assetTitles{assets: d.Assets, known: map[string]string{}}
}

// grounding is sources, named by their assets' titles, in order;
// exploratory for none. An asset that can't be read, or is gone, still
// counts by its id.
func (t *assetTitles) grounding(ctx context.Context, sources []assistantkit.Source) assistantkit.Grounding {
	var missing []string
	for _, s := range sources {
		if _, ok := t.known[s.ID]; !ok && !slices.Contains(missing, s.ID) {
			missing = append(missing, s.ID)
		}
	}
	if len(missing) > 0 && t.assets != nil {
		if metas, err := t.assets.ListMeta(ctx, missing); err == nil {
			for _, m := range metas {
				t.known[m.ID] = m.Title
			}
		}
	}
	for i := range sources {
		sources[i].Title = t.known[sources[i].ID]
	}
	return assistantkit.GroundingFrom(sources)
}

// fromIdeas is the assets ids name as sources, each traced to the first of
// ideas that drew on it: the idea, and the analysis when the idea was
// distilled from that asset.
func fromIdeas(ids []string, ideas []models.Idea) []assistantkit.Source {
	sources := make([]assistantkit.Source, len(ids))
	for i, id := range ids {
		sources[i].ID = id
		for _, idea := range ideas {
			if idea.SourceAssetID != nil && *idea.SourceAssetID == id {
				sources[i].IdeaID = idea.ID
				if idea.AssetAnalysisID != nil {
					sources[i].AnalysisID = *idea.AssetAnalysisID
				}
				break
			}
			if slices.Contains(idea.AssetIDs, id) {
				sources[i].IdeaID = idea.ID
				break
			}
		}
	}
	return sources
}

// orphaned reports a compile or draft that found no campaign to use: the
// ideas are in none, or in several. In a campaign thread the thread's
// campaign is then the one.
func orphaned(err error) bool {
	return errors.Is(err, compose.ErrNoCampaign) || errors.Is(err, compose.ErrMixedCampaigns)
}

// DraftIdeasInput writes posts from ideas.
type DraftIdeasInput struct {
	IdeaIDs     []string `json:"ideaIds,omitempty"     jsonschema:"description=Default the ideas selected on screen, else the open one"`
	PlatformIDs []string `json:"platformIds,omitempty" jsonschema:"description=Platforms to write for; default the ideas' suggested platform"`
	PostType    string   `json:"postType,omitempty"    jsonschema:"description=Needs exactly one platform"`
	Together    bool     `json:"together,omitempty"    jsonschema:"description=One post per platform from all the ideas, instead of one per idea"`
	Instruction string   `json:"instruction,omitempty" jsonschema:"description=Steering for the writer, e.g. punchier, for CFOs"`
	TargetDate  string   `json:"targetDate,omitempty"  jsonschema:"description=YYYY-MM-DD; only when the user names a date, else the drafts stay unscheduled"`
	CampaignID  string   `json:"campaignId,omitempty"`
}

// DraftIdeasOutput is the written posts.
type DraftIdeasOutput struct {
	Posts    []DraftedIdeaPost `json:"posts"`
	Warnings []string          `json:"warnings,omitempty"`
	// retry and notMade are what a run stopped by an error left unwritten.
	retry   *DraftIdeasInput
	notMade []assistantkit.NotMade
	assistantkit.Partial
}

// DraftedIdeaPost is a post written from ideas and what it is grounded in:
// the ideas' assets the writer was given, or exploratory without any. It
// is also each idea_draft_post event.
type DraftedIdeaPost struct {
	compose.DraftedPost
	Grounding assistantkit.Grounding `json:"grounding"`
}

func planDraft(ctx context.Context, d Deps, in DraftIdeasInput) (*compose.DraftPlan, error) {
	date, err := calendarDate(in.TargetDate)
	if err != nil {
		return nil, err
	}
	plan := func(campaignID string) (*compose.DraftPlan, error) {
		return d.Compose.PlanDraft(ctx, compose.DraftInput{
			IdeaIDs: in.IdeaIDs, CampaignID: campaignID, PlatformIDs: in.PlatformIDs,
			PostType: in.PostType, TargetDate: date, Instruction: in.Instruction, Together: in.Together, Actor: actor(ctx),
		}, max(d.DraftMax, 1))
	}
	p, err := plan(in.CampaignID)
	if thread := assistant.CampaignID(ctx); in.CampaignID == "" && thread != "" && orphaned(err) {
		p, err = plan(thread)
	}
	return p, composeErr(err)
}

func draftIdeas(ctx context.Context, d Deps, in DraftIdeasInput) (*DraftIdeasOutput, error) {
	plan, err := planDraft(ctx, d, in)
	if err != nil {
		return nil, err
	}
	titles := newAssetTitles(d)
	out := &DraftIdeasOutput{Posts: []DraftedIdeaPost{}}
	res, err := d.Compose.RunDraft(ctx, plan, func(p compose.DraftedPost) {
		assistantkit.Touch(ctx, "post", p.PostID)
		ideas := slices.DeleteFunc(slices.Clone(plan.Ideas), func(i models.Idea) bool { return !slices.Contains(p.IdeaIDs, i.ID) })
		g := titles.grounding(ctx, fromIdeas(p.UsedAssetIDs, ideas))
		if len(plan.Jobs) > 0 {
			g = analysed(ctx, d, &plan.Jobs[0].Request.CampaignID, g)
		}
		drafted := DraftedIdeaPost{DraftedPost: p, Grounding: g}
		out.Posts = append(out.Posts, drafted)
		assistant.Emit(ctx, "idea_draft_post", drafted)
	})
	if err != nil && (res == nil || len(res.Posts) == 0) {
		return nil, err
	}
	out.Warnings = res.Warnings
	if err != nil {
		out.Warnings = append(out.Warnings, "stopped early: "+err.Error())
		out.retry, out.notMade = unwritten(plan, res.Posts, in, "stopped early: "+err.Error())
	}
	return out, nil
}

// draftRetry retries the posts a stopped draft left unwritten.
func draftRetry(_ DraftIdeasInput, out *DraftIdeasOutput) (DraftIdeasInput, []assistantkit.NotMade, bool) {
	if out.retry == nil {
		return DraftIdeasInput{}, out.notMade, false
	}
	return *out.retry, out.notMade, true
}

// unwritten is the jobs of plan no post came from, each one idea (or all of
// them, together) for one platform, and the call that writes them again. A
// stop leaves a tail of the jobs, which a call covers exactly when it is
// whole ideas, or whole platforms; otherwise the call covers the ideas
// nothing was written from, and the rest is only reported.
func unwritten(plan *compose.DraftPlan, written []compose.DraftedPost, in DraftIdeasInput, reason string) (*DraftIdeasInput, []assistantkit.NotMade) {
	type job struct{ idea, platform string }
	done := map[job]bool{}
	for _, p := range written {
		done[job{p.IdeaIDs[0], p.PlatformID}] = true
	}
	titles := map[string]string{}
	for _, i := range plan.Ideas {
		titles[i.ID] = i.Title
	}
	var notMade []assistantkit.NotMade
	var ideaIDs, platforms []string
	wrote := map[string]bool{}
	for _, j := range plan.Jobs {
		key := job{j.IdeaIDs[0], j.Request.PlatformID}
		if done[key] {
			wrote[key.idea] = true
			continue
		}
		notMade = append(notMade, assistantkit.NotMade{ID: key.idea, Title: titles[key.idea] + " · " + j.Request.PlatformID, Reason: reason})
		if !slices.Contains(ideaIDs, key.idea) {
			ideaIDs = append(ideaIDs, key.idea)
		}
		if !slices.Contains(platforms, key.platform) {
			platforms = append(platforms, key.platform)
		}
	}
	if len(notMade) == 0 {
		return nil, nil
	}
	retry := in
	retry.PlatformIDs = platforms
	if !in.Together {
		retry.IdeaIDs = ideaIDs
		if len(ideaIDs)*len(platforms) != len(notMade) {
			retry.IdeaIDs = slices.DeleteFunc(ideaIDs, func(id string) bool { return wrote[id] })
			retry.PlatformIDs = in.PlatformIDs
		}
	}
	if len(retry.IdeaIDs) == 0 {
		return nil, notMade
	}
	return &retry, notMade
}
