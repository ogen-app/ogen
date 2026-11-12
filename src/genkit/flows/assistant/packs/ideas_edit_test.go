package packs

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/ideas"
)

// An edit changes only what it names, logs the before and after for undo,
// and refuses an analysed idea's text, a bad date and an empty edit.
func TestEditIdea(t *testing.T) {
	d, repo, actions := ideaDeps()
	day, _ := models.ParseCalendarDate("2026-12-02")
	repo.byID["i1"].Plan = models.IdeaPlan{PlatformID: new("x"), TargetDate: &day}
	ctx, touched := turn(t, workspace, nil)

	out, err := editIdea(ctx, d, EditIdeaInput{IdeaID: "i1", Title: new(" Five pricing myths "),
		Plan: &PlanEdit{PlatformID: new("linkedin"), TargetDate: new("")}})
	if err != nil {
		t.Fatal(err)
	}
	got := repo.byID["i1"]
	if got.Title != "Five pricing myths" || *got.Plan.PlatformID != "linkedin" || got.Plan.TargetDate != nil || out.UndoID == "" {
		t.Fatalf("edited = %+v, plan %+v; out %+v", got, got.Plan, out)
	}
	if !slices.Equal(out.Changed, []string{"title", "plan"}) || len(touched()) != 1 {
		t.Fatalf("changed = %v, touched %v", out.Changed, touched())
	}
	if out.Before.Title != "Pricing myths" || *out.Before.Plan.PlatformID != "x" || out.Before.Plan.TargetDate == nil {
		t.Fatalf("before = %+v", out.Before)
	}
	a := actions.rows[0]
	if a.Kind != models.AssistantActionIdeaEdit || a.Before["title"] != "Pricing myths" || a.After["title"] != "Five pricing myths" {
		t.Fatalf("action = %+v", a)
	}

	repo.byID["i2"].Origin = models.IdeaOriginAssetAnalysis
	_, err = editIdea(ctx, d, EditIdeaInput{IdeaID: "i2", Note: new("rewritten")})
	if r, ok := errors.AsType[*assistantkit.Refusal](err); !ok || r.Code != "idea_read_only" {
		t.Fatalf("analysed idea's note = %v; want the idea_read_only refusal", err)
	}
	if _, err := editIdea(ctx, d, EditIdeaInput{IdeaID: "i2", Plan: &PlanEdit{PostType: new("carousel")}}); err != nil {
		t.Fatalf("an analysed idea's plan is editable: %v", err)
	}
	for _, in := range []EditIdeaInput{{IdeaID: "i1"}, {}, {IdeaID: "i1", Plan: &PlanEdit{TargetDate: new("next week")}},
		{IdeaID: "i1", Plan: &PlanEdit{PhaseID: new("launch")}}, {IdeaID: "gone", Title: new("x")}} {
		_, err := editIdea(ctx, d, in)
		refused(t, err)
	}
}

// A bulk triage says what it set and names each idea it changed, with the
// date a later one comes back; a move names the ideas it moved.
func TestSetIdeaVerdicts_SaysWhatChanged(t *testing.T) {
	d, _, _ := ideaDeps()
	ctx, _ := turn(t, workspace, nil)
	back := time.Now().AddDate(0, 1, 0).Format(time.DateOnly)
	out, err := setIdeaVerdicts(ctx, d, SetIdeaVerdictsInput{IdeaIDs: []string{"i1", "i2", "gone"}, Verdict: "later", RemindAt: back})
	if err != nil {
		t.Fatal(err)
	}
	if out.Verdict != "later" || out.RemindAt == nil || out.RemindAt.Format(time.DateOnly) != back || len(out.Ideas) != 2 {
		t.Fatalf("verdicts = %+v", out)
	}
	if i := out.Ideas[1]; i.ID != "i2" || i.Title != "Founder story" || i.Verdict != "later" || i.RemindAt == nil {
		t.Fatalf("row = %+v", i)
	}
	if out, _ = setIdeaVerdicts(ctx, d, SetIdeaVerdictsInput{IdeaIDs: []string{"i1"}, Verdict: "none"}); out.Verdict != "none" || out.Ideas[0].Verdict != "" {
		t.Fatalf("back to the inbox = %+v", out)
	}
	moved, err := moveIdeas(ctx, d, MoveIdeasInput{IdeaIDs: []string{"i1"}})
	if err != nil || len(moved.Ideas) != 1 || moved.Ideas[0].Title != "Pricing myths" || moved.Verdict != "" {
		t.Fatalf("move = %+v, %v", moved, err)
	}
}

// similarIdeas reads the ideas like one, each with why.
func TestSimilarIdeas(t *testing.T) {
	d, repo, _ := ideaDeps()
	repo.byID["i3"] = &models.Idea{ID: "i3", Title: "Pricing myths, busted"}
	d.Ideas = ideas.New(similarIdeaRepo{repo}, nil)
	ctx, _ := turn(t, workspace, nil)
	out, err := similarIdeas(ctx, d, SimilarIdeasInput{IdeaID: "i1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Title != "Pricing myths" || len(out.Similar) != 1 || out.Similar[0].Idea.ID != "i3" || !strings.Contains(out.Similar[0].Why[0], "myth, pricing") {
		t.Fatalf("similar = %+v", out)
	}
	_, err = similarIdeas(ctx, d, SimilarIdeasInput{})
	refused(t, err)
}

// similarIdeaRepo lists every kept idea to a query.
type similarIdeaRepo struct{ *memIdeaRepo }

func (r similarIdeaRepo) Query(context.Context, models.IdeaQuery) (*models.IdeaQueryResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []models.Idea
	for _, id := range slices.Sorted(maps.Keys(r.byID)) {
		out = append(out, *r.byID[id])
	}
	return &models.IdeaQueryResult{Ideas: out}, nil
}
