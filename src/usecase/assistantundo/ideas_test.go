package assistantundo_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
)

// memIdeas holds one idea and compares-and-sets its triage as the
// repository does.
type memIdeas struct {
	repository.IdeaRepository
	i *models.Idea
}

func eq[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func eqTime(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

func (m *memIdeas) SetTriageIf(_ context.Context, _ bun.IDB, i, was *models.Idea) (bool, error) {
	c := m.i
	if c == nil || c.ID != i.ID || !eq(c.Verdict, was.Verdict) || !eqTime(c.RemindAt, was.RemindAt) ||
		!eqTime(c.DecidedAt, was.DecidedAt) || !eq(c.DecidedBy, was.DecidedBy) {
		return false, nil
	}
	c.Verdict, c.RemindAt, c.DecidedAt, c.DecidedBy = i.Verdict, i.RemindAt, i.DecidedAt, i.DecidedBy
	return true, nil
}

// logVerdict logs a verdict change on ideas' idea, as setIdeaVerdict does.
func logVerdict(t *testing.T, actions *memActions, ideas *memIdeas, change func(*models.Idea)) string {
	t.Helper()
	before := assistantundo.IdeaVerdict(ideas.i)
	change(ideas.i)
	id, err := assistantundo.Record(t.Context(), actions, &models.AssistantAction{
		Kind: models.AssistantActionIdeaVerdict, EntityID: ideas.i.ID,
		Before: before, After: assistantundo.IdeaVerdict(ideas.i), CreatedBy: "u2",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUndo_IdeaVerdict(t *testing.T) {
	ideas := &memIdeas{i: &models.Idea{ID: "i1"}}
	actions := &memActions{}
	decided := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	id := logVerdict(t, actions, ideas, func(i *models.Idea) {
		i.Verdict, i.DecidedAt, i.DecidedBy = new(models.IdeaVerdictYes), &decided, new("u2")
	})
	svc := assistantundo.New(actions, &memCampaigns{}).WithIdeas(ideas)

	// Changed again since: refused.
	ideas.i.Verdict = new(models.IdeaVerdictNo)
	if _, err := svc.Undo(t.Context(), id, "u1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("changed since: %v", err)
	}
	// The same verdict decided again by someone else is a change too.
	ideas.i.Verdict, ideas.i.DecidedBy = new(models.IdeaVerdictYes), new("u3")
	if _, err := svc.Undo(t.Context(), id, "u1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("decided again: %v", err)
	}
	ideas.i.DecidedBy = new("u2")
	if _, err := svc.Undo(t.Context(), id, "u1"); err != nil {
		t.Fatal(err)
	}
	if ideas.i.Verdict != nil || ideas.i.DecidedAt != nil || ideas.i.DecidedBy != nil {
		t.Fatalf("idea = %+v; want back in the inbox", ideas.i)
	}
}

// Undo puts back the verdict, the reminder and the decision it replaced, not
// just clears them.
func TestUndo_IdeaVerdictRestoresEarlierTriage(t *testing.T) {
	remind := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	decided := time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)
	ideas := &memIdeas{i: &models.Idea{
		ID: "i1", Verdict: new(models.IdeaVerdictLater), RemindAt: &remind, DecidedAt: &decided, DecidedBy: new("u1"),
	}}
	actions := &memActions{}
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	id := logVerdict(t, actions, ideas, func(i *models.Idea) {
		i.Verdict, i.RemindAt, i.DecidedAt, i.DecidedBy = new(models.IdeaVerdictYes), nil, &now, new("u2")
	})

	if _, err := assistantundo.New(actions, &memCampaigns{}).WithIdeas(ideas).Undo(t.Context(), id, "u2"); err != nil {
		t.Fatal(err)
	}
	i := ideas.i
	if !eq(i.Verdict, new(models.IdeaVerdictLater)) || !eqTime(i.RemindAt, &remind) {
		t.Fatalf("verdict = %v, remind_at = %v; want later until %v", i.Verdict, i.RemindAt, remind)
	}
	if !eqTime(i.DecidedAt, &decided) || !eq(i.DecidedBy, new("u1")) {
		t.Fatalf("decided = %v by %v; want the original decision", i.DecidedAt, i.DecidedBy)
	}
}

func (m *memCampaigns) SetAssetIDsIf(_ context.Context, _ bun.IDB, id string, was []string, wasUse bool, ids []string, use bool) (bool, error) {
	if m.c == nil || m.c.ID != id || !slices.Equal([]string(m.c.AssetIDs), was) || m.c.UseAssets != wasUse {
		return false, nil
	}
	m.c.AssetIDs, m.c.UseAssets = ids, use
	return true, nil
}

func TestUndo_CampaignAssets(t *testing.T) {
	camps := &memCampaigns{c: &models.Campaign{ID: "c1", AssetIDs: models.StringSlice{"a1"}, UseAssets: true}}
	before := assistantundo.CampaignAssets(camps.c)
	camps.c.AssetIDs = models.StringSlice{"a1", "a2"}
	after := assistantundo.CampaignAssets(camps.c)

	actions := &memActions{}
	id, err := assistantundo.Record(t.Context(), actions, &models.AssistantAction{Kind: models.AssistantActionCampaignAssets, EntityID: "c1", Before: before, After: after})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assistantundo.New(actions, camps).Undo(t.Context(), id, "u1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(camps.c.AssetIDs, models.StringSlice{"a1"}) || !camps.c.UseAssets {
		t.Fatalf("assets = %v, use = %v", camps.c.AssetIDs, camps.c.UseAssets)
	}
}

// Undoing a first attach turns the campaign's assets off again, and a flag
// flipped since the change counts as an edit.
func TestUndo_CampaignAssetsUseAssets(t *testing.T) {
	camps := &memCampaigns{c: &models.Campaign{ID: "c1", AssetIDs: models.StringSlice{}}}
	before := assistantundo.CampaignAssets(camps.c)
	camps.c.AssetIDs, camps.c.UseAssets = models.StringSlice{"a1"}, true
	after := assistantundo.CampaignAssets(camps.c)

	actions := &memActions{}
	id, err := assistantundo.Record(t.Context(), actions, &models.AssistantAction{Kind: models.AssistantActionCampaignAssets, EntityID: "c1", Before: before, After: after})
	if err != nil {
		t.Fatal(err)
	}
	svc := assistantundo.New(actions, camps)
	camps.c.UseAssets = false
	if _, err := svc.Undo(t.Context(), id, "u1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("flag changed since: %v", err)
	}
	camps.c.UseAssets = true
	if _, err := svc.Undo(t.Context(), id, "u1"); err != nil {
		t.Fatal(err)
	}
	if len(camps.c.AssetIDs) != 0 || camps.c.UseAssets {
		t.Fatalf("assets = %v, use = %v; want none, off", camps.c.AssetIDs, camps.c.UseAssets)
	}
}
