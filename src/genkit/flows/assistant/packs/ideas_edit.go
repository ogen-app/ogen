package packs

import (
	"context"
	"errors"
	"strings"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/ideas"
)

// defineEditTools defines editIdea and similarIdeas.
func defineEditTools(g *genkit.Genkit, d Deps) []assistantkit.Spec {
	edit := genkit.DefineTool(g, "editIdea",
		"Edits one idea's title, note or plan (platform, post type, phase, target date). Returns an undo id.",
		onScreen(func(ctx context.Context, in *EditIdeaInput) { in.IdeaID = ideaOnScreen(ctx, in.IdeaID) },
			assistantkit.Wrap(recorded(ideasPackName, "editIdea", nil, func(ctx context.Context, in EditIdeaInput) (*EditIdeaOutput, error) {
				return editIdea(ctx, d, in)
			}))), ai.WithStrictSchema(false))
	similar := genkit.DefineTool(g, "similarIdeas",
		"Finds ideas like one idea: those sharing its words, its source asset or assets, most alike first, each with why.",
		onScreen(func(ctx context.Context, in *SimilarIdeasInput) { in.IdeaID = ideaOnScreen(ctx, in.IdeaID) },
			assistantkit.Wrap(recorded(ideasPackName, "similarIdeas", nil, func(ctx context.Context, in SimilarIdeasInput) (*SimilarIdeasOutput, error) {
				return similarIdeas(ctx, d, in)
			}))), ai.WithStrictSchema(false))
	return []assistantkit.Spec{
		spec(ideasPackName, edit, assistantkit.TierReversible,
			`Call this to change one idea's title, note or plan — "rename it to …", "plan it for LinkedIn next Tuesday", "drop its date". ideaId defaults to the idea open on screen, else the one selected. Pass only what changes; a plan field set to "" clears it. An analysed idea's title and note can't be edited, only its plan. To move it to another campaign use moveIdeas. It can be undone.`),
		spec(ideasPackName, similar, assistantkit.TierRead,
			`Call this when the user asks for ideas like one idea — "anything similar to this?", "more like this one". ideaId defaults to the idea open on screen, else the one selected. It searches this thread's campaign, or every idea outside one. For near-identical ideas to merge use findDuplicateIdeas instead.`),
	}
}

// EditIdeaInput changes one idea; fields left out stay as they are.
type EditIdeaInput struct {
	IdeaID string    `json:"ideaId,omitempty" jsonschema:"description=Defaults to the idea open on screen, else the one selected"`
	Title  *string   `json:"title,omitempty"`
	Note   *string   `json:"note,omitempty"`
	Plan   *PlanEdit `json:"plan,omitempty" jsonschema:"description=The plan fields to change; \"\" clears one"`
}

// IdeaText is an idea's editable fields.
type IdeaText struct {
	Title string          `json:"title"`
	Note  string          `json:"note,omitempty"`
	Plan  models.IdeaPlan `json:"plan"`
}

// ideaReadOnlyCode refuses an edit to an analysed idea's words, as the
// ideas API's 409 does.
const ideaReadOnlyCode = "idea_read_only"

// PlanEdit changes an idea's plan field by field: nil keeps a field, ""
// clears it.
type PlanEdit struct {
	PlatformID *string `json:"platformId,omitempty"`
	PostType   *string `json:"postType,omitempty"`
	PhaseID    *string `json:"phaseId,omitempty"    jsonschema:"description=A phase of the idea's campaign's type"`
	TargetDate *string `json:"targetDate,omitempty" jsonschema:"description=YYYY-MM-DD"`
}

// EditIdeaOutput is the idea after the edit, and before it.
type EditIdeaOutput struct {
	Idea    IdeaRef  `json:"idea"`
	Note    string   `json:"note,omitempty"`
	Before  IdeaText `json:"before"`
	Changed []string `json:"changed"`
	UndoID  string   `json:"undoId,omitempty"`
}

func editIdea(ctx context.Context, d Deps, in EditIdeaInput) (*EditIdeaOutput, error) {
	if in.IdeaID == "" {
		return nil, assistantkit.Refused("which idea? open it, select it, or pass ideaId")
	}
	if in.Title == nil && in.Note == nil && in.Plan == nil {
		return nil, assistantkit.Refused("nothing to change; pass a title, note or plan")
	}
	before, err := d.Ideas.Get(ctx, in.IdeaID)
	if err != nil {
		return nil, triageErr(err)
	}
	up := ideas.UpdateInput{Title: in.Title, Note: in.Note}
	if in.Plan != nil {
		if up.Plan, err = editedPlan(before.Plan, *in.Plan); err != nil {
			return nil, err
		}
		up.SetPlan = true
	}
	idea, columns, err := d.Ideas.Update(ctx, in.IdeaID, up)
	if errors.Is(err, ideas.ErrReadOnly) {
		return nil, assistantkit.RefusedCode(ideaReadOnlyCode, "an analysed idea's title and note can't be edited: its fit score rests on them. Its plan can be")
	}
	if err != nil {
		return nil, triageErr(err)
	}
	assistantkit.Touch(ctx, "idea", idea.ID)
	return &EditIdeaOutput{Idea: ideaRef(*idea), Note: idea.Note, Changed: changedFields(columns),
		Before: IdeaText{Title: before.Title, Note: before.Note, Plan: before.Plan},
		UndoID: recordUndo(ctx, d, models.AssistantActionIdeaEdit, idea.ID, assistantundo.IdeaEdit(before), assistantundo.IdeaEdit(idea))}, nil
}

// editedPlan is p with e's fields applied: a nil field kept, "" cleared.
func editedPlan(p models.IdeaPlan, e PlanEdit) (models.IdeaPlan, error) {
	set := func(dst **string, v *string) {
		if v == nil {
			return
		}
		if s := strings.TrimSpace(*v); s != "" {
			*dst = &s
		} else {
			*dst = nil
		}
	}
	set(&p.PlatformID, e.PlatformID)
	set(&p.PostType, e.PostType)
	set(&p.PhaseID, e.PhaseID)
	if e.TargetDate != nil {
		date, err := calendarDate(strings.TrimSpace(*e.TargetDate))
		if err != nil {
			return p, err
		}
		p.TargetDate = date
	}
	return p, nil
}

// changedFields names the edit's fields from the columns it wrote.
func changedFields(columns []string) []string {
	out := []string{}
	plan := false
	for _, c := range columns {
		switch {
		case strings.HasPrefix(c, "plan_"):
			if !plan {
				out, plan = append(out, "plan"), true
			}
		default:
			out = append(out, c)
		}
	}
	return out
}

// SimilarIdeasInput names the idea to find others like.
type SimilarIdeasInput struct {
	IdeaID string `json:"ideaId,omitempty" jsonschema:"description=Defaults to the idea open on screen, else the one selected"`
	Limit  int    `json:"limit,omitempty"  jsonschema:"description=How many; default 10, at most 20"`
}

// SimilarIdeasOutput is the ideas like one idea, the most alike first.
type SimilarIdeasOutput struct {
	IdeaID  string        `json:"ideaId"`
	Title   string        `json:"title"`
	Similar []SimilarIdea `json:"similar"`
}

// SimilarIdea is one idea like it, how alike (0–1) and why.
type SimilarIdea struct {
	Idea  IdeaRef  `json:"idea"`
	Score float64  `json:"score"`
	Why   []string `json:"why"`
}

func similarIdeas(ctx context.Context, d Deps, in SimilarIdeasInput) (*SimilarIdeasOutput, error) {
	if in.IdeaID == "" {
		return nil, assistantkit.Refused("which idea? open it, select it, or pass ideaId")
	}
	idea, err := d.Ideas.Get(ctx, in.IdeaID)
	if err != nil {
		return nil, triageErr(err)
	}
	limit := 10
	if in.Limit > 0 {
		limit = in.Limit
	}
	found, err := d.Ideas.Similar(ctx, in.IdeaID, assistant.CampaignID(ctx), limit)
	if errors.Is(err, ideas.ErrScopeNotFound) {
		return nil, assistantkit.Refused("this thread's campaign is gone")
	}
	if err != nil {
		return nil, triageErr(err)
	}
	out := &SimilarIdeasOutput{IdeaID: idea.ID, Title: idea.Title, Similar: []SimilarIdea{}}
	for _, s := range found {
		var why []string
		if len(s.Terms) > 0 {
			why = append(why, "shares the words "+strings.Join(s.Terms, ", "))
		}
		if s.SameSource {
			why = append(why, "comes from the same asset")
		} else if len(s.SharedAssets) > 0 {
			why = append(why, "draws on the same assets")
		}
		if s.SameCampaign {
			why = append(why, "is in the same campaign")
		}
		out.Similar = append(out.Similar, SimilarIdea{Idea: ideaRef(s.Idea), Score: s.Score, Why: why})
	}
	return out, nil
}
