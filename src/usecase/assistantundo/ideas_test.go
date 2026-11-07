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

	f := newFixture(t)
	id, err := assistantundo.Record(f.ctx, f.actions, &models.AssistantAction{ScopeKind: models.AssistantScopeWorkspace,
		Kind: models.AssistantActionIdeaVerdict, EntityID: "i1", Before: before, After: after, CreatedBy: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	svc := assistantundo.New(f.db, f.actions, f.campaigns).WithIdeas(ideas)

	// Changed again since: refused.
	ideas.i.Verdict = new(models.IdeaVerdictNo)
	if _, err := svc.Undo(f.ctx, id, "u1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("changed since: %v", err)
	}
	ideas.i.Verdict = new(models.IdeaVerdictYes)
	if _, err := svc.Undo(f.ctx, id, "u1"); err != nil {
		t.Fatal(err)
	}
	if ideas.i.Verdict != nil || ideas.i.DecidedAt != nil {
		t.Fatalf("idea = %+v; want back in the inbox", ideas.i)
	}
}

func TestUndo_CampaignAssets(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, func(c *models.Campaign) { c.AssetIDs = models.StringSlice{"a1"} })
	id := f.logChange(t, models.AssistantActionCampaignAssets, assistantundo.CampaignAssets, func(c *models.Campaign) {
		c.AssetIDs = models.StringSlice{"a1", "a2"}
	})

	if _, err := f.svc.Undo(f.ctx, id, "u1"); err != nil {
		t.Fatal(err)
	}
	if got := f.load(t).AssetIDs; !slices.Equal(got, models.StringSlice{"a1"}) {
		t.Fatalf("assets = %v", got)
	}
}
