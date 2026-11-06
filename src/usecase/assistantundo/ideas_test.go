package assistantundo_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
)

type memIdeas struct {
	repository.IdeaRepository
	i *models.Idea
}

func (m *memIdeas) GetByID(context.Context, string) (*models.Idea, error) {
	if m.i == nil {
		return nil, sql.ErrNoRows
	}
	cp := *m.i
	return &cp, nil
}

func (m *memIdeas) Update(_ context.Context, i *models.Idea, _ ...string) (bool, error) {
	cp := *i
	m.i = &cp
	return true, nil
}

func TestUndo_IdeaVerdict(t *testing.T) {
	ideas := &memIdeas{i: &models.Idea{ID: "i1"}}
	before := assistantundo.IdeaVerdict(ideas.i)
	ideas.i.Verdict = new(models.IdeaVerdictYes)
	after := assistantundo.IdeaVerdict(ideas.i)

	actions := &memActions{}
	id, err := assistantundo.Record(t.Context(), actions, &models.AssistantAction{Kind: models.AssistantActionIdeaVerdict, EntityID: "i1", Before: before, After: after, CreatedBy: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	svc := assistantundo.New(actions, &memCampaigns{}).WithIdeas(ideas)

	// Changed again since: refused.
	ideas.i.Verdict = new(models.IdeaVerdictNo)
	if _, err := svc.Undo(t.Context(), id, "u1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("changed since: %v", err)
	}
	ideas.i.Verdict = new(models.IdeaVerdictYes)
	if _, err := svc.Undo(t.Context(), id, "u1"); err != nil {
		t.Fatal(err)
	}
	if ideas.i.Verdict != nil || ideas.i.DecidedAt != nil {
		t.Fatalf("idea = %+v; want back in the inbox", ideas.i)
	}
}

func TestUndo_CampaignAssets(t *testing.T) {
	camps := &memCampaigns{c: &models.Campaign{ID: "c1", AssetIDs: models.StringSlice{"a1"}}}
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
	if !slices.Equal(camps.c.AssetIDs, models.StringSlice{"a1"}) {
		t.Fatalf("assets = %v", camps.c.AssetIDs)
	}
}
