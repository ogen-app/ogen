package assistantundo_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
)

type memActions struct {
	mu   sync.Mutex
	rows map[string]*models.AssistantAction
}

func (m *memActions) Create(_ context.Context, a *models.AssistantAction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]*models.AssistantAction{}
	}
	m.rows[a.ID] = a
	return nil
}

func (m *memActions) GetByID(_ context.Context, id string) (*models.AssistantAction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.rows[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	cp := *a
	return &cp, nil
}

func (m *memActions) MarkUndone(_ context.Context, id, by string, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.rows[id]
	if !ok || a.UndoneAt != nil {
		return false, nil
	}
	a.UndoneAt, a.UndoneBy = &now, by
	return true, nil
}

type memCampaigns struct {
	repository.CampaignRepository
	c *models.Campaign
}

func (m *memCampaigns) GetByID(context.Context, string) (*models.Campaign, error) {
	if m.c == nil {
		return nil, sql.ErrNoRows
	}
	cp := *m.c
	return &cp, nil
}

func (m *memCampaigns) Update(_ context.Context, c *models.Campaign, _ ...string) error {
	m.c = c
	return nil
}

func day(s string) *time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return &t
}

// logChange applies change to the stored campaign and logs it, as a tool does.
func logChange(t *testing.T, actions *memActions, campaigns *memCampaigns, kind models.AssistantActionKind,
	snapshot func(*models.Campaign) models.JSONMap, change func(*models.Campaign),
) string {
	t.Helper()
	before := snapshot(campaigns.c)
	change(campaigns.c)
	id, err := assistantundo.Record(t.Context(), actions, &models.AssistantAction{
		ScopeKind: models.AssistantScopeCampaign, ScopeID: "c1", Kind: kind, EntityID: "c1",
		Before: before, After: snapshot(campaigns.c),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUndo_CampaignDates(t *testing.T) {
	actions := &memActions{}
	campaigns := &memCampaigns{c: &models.Campaign{ID: "c1", EndDate: day("2026-06-30")}}
	svc := assistantundo.New(actions, campaigns)
	id := logChange(t, actions, campaigns, models.AssistantActionCampaignDates, assistantundo.CampaignDates, func(c *models.Campaign) {
		c.StartDate, c.EndDate = day("2026-06-01"), day("2026-07-31")
	})

	a, err := svc.Undo(t.Context(), id, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if campaigns.c.StartDate != nil || !campaigns.c.EndDate.Equal(*day("2026-06-30")) {
		t.Fatalf("restored dates = %v, %v", campaigns.c.StartDate, campaigns.c.EndDate)
	}
	if a.UndoneBy != "user-1" || a.UndoneAt == nil {
		t.Fatalf("action = %+v", a)
	}
	if _, err := svc.Undo(t.Context(), id, "user-1"); !errors.Is(err, assistantundo.ErrAlreadyUndone) {
		t.Fatalf("second undo err = %v", err)
	}
}

func TestUndo_CampaignBrief(t *testing.T) {
	actions := &memActions{}
	campaigns := &memCampaigns{c: &models.Campaign{ID: "c1", Description: "old", KeyMessages: "km"}}
	svc := assistantundo.New(actions, campaigns)
	id := logChange(t, actions, campaigns, models.AssistantActionCampaignBrief, assistantundo.CampaignBrief, func(c *models.Campaign) {
		c.Description, c.TargetPersona, c.KeyMessages, c.ToneGuidelines = "new", "persona", "km2", "calm"
	})

	if _, err := svc.Undo(t.Context(), id, "user-1"); err != nil {
		t.Fatal(err)
	}
	c := campaigns.c
	if c.Description != "old" || c.TargetPersona != "" || c.KeyMessages != "km" || c.ToneGuidelines != "" {
		t.Fatalf("restored brief = %+v", c)
	}
}

// An edit made after the assistant's change is never overwritten.
func TestUndo_RefusesWhenChangedSince(t *testing.T) {
	actions := &memActions{}
	campaigns := &memCampaigns{c: &models.Campaign{ID: "c1", Description: "old"}}
	svc := assistantundo.New(actions, campaigns)
	id := logChange(t, actions, campaigns, models.AssistantActionCampaignBrief, assistantundo.CampaignBrief, func(c *models.Campaign) {
		c.Description = "assistant"
	})
	campaigns.c.Description = "user edit"

	if _, err := svc.Undo(t.Context(), id, "user-1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("err = %v, want ErrChangedSince", err)
	}
	if campaigns.c.Description != "user edit" {
		t.Fatalf("description = %q, the later edit was overwritten", campaigns.c.Description)
	}
	if a, _ := actions.GetByID(t.Context(), id); a.UndoneAt != nil {
		t.Fatal("a refused undo was recorded")
	}
}

func TestUndo_CampaignPlatforms(t *testing.T) {
	actions := &memActions{}
	campaigns := &memCampaigns{c: &models.Campaign{ID: "c1", TargetPlatforms: models.CampaignPlatforms{{ID: "pl1", PostTypes: []string{"article"}}}}}
	svc := assistantundo.New(actions, campaigns)
	id := logChange(t, actions, campaigns, models.AssistantActionCampaignPlatforms, assistantundo.CampaignPlatforms, func(c *models.Campaign) {
		c.TargetPlatforms = append(c.TargetPlatforms, models.CampaignPlatform{ID: "pl2"})
	})

	if _, err := svc.Undo(t.Context(), id, "user-1"); err != nil {
		t.Fatal(err)
	}
	tp := campaigns.c.TargetPlatforms
	if len(tp) != 1 || tp[0].ID != "pl1" || len(tp[0].PostTypes) != 1 || tp[0].PostTypes[0] != "article" {
		t.Fatalf("restored platforms = %+v", tp)
	}
}

func TestUndo_NotFound(t *testing.T) {
	svc := assistantundo.New(&memActions{}, &memCampaigns{})
	if _, err := svc.Undo(t.Context(), "nope", "user-1"); !errors.Is(err, assistantundo.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
