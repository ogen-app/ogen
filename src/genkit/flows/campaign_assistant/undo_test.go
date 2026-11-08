package campaign_assistant

import (
	"context"
	"database/sql"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/enrich_brief"
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
	if a, ok := m.rows[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, sql.ErrNoRows
}

func (m *memActions) MarkUndone(_ context.Context, _ bun.IDB, id, by string, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.rows[id]
	if !ok || a.UndoneAt != nil {
		return false, nil
	}
	a.UndoneAt, a.UndoneBy = &now, by
	return true, nil
}

// storedCampaign is a campaign repository holding one campaign that tools
// write through.
type storedCampaign struct {
	repository.CampaignRepository
	c *models.Campaign
	// columns lists what each UpdateColumns call wrote; failUpdate makes it
	// fail.
	columns    [][]string
	failUpdate error
}

// UpdateColumns stores only the platform column, the one narrow write the
// tools make.
func (s *storedCampaign) UpdateColumns(_ context.Context, _ bun.IDB, c *models.Campaign, columns ...string) error {
	s.columns = append(s.columns, columns)
	if s.failUpdate != nil {
		return s.failUpdate
	}
	cp := *s.c
	if slices.Contains(columns, "target_platforms") {
		cp.TargetPlatforms = c.TargetPlatforms
	}
	s.c = &cp
	return nil
}

// logged returns the action the tool logged.
func (m *memActions) logged(t *testing.T, id string) *models.AssistantAction {
	t.Helper()
	a, err := m.GetByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (s *storedCampaign) GetByID(context.Context, string) (*models.Campaign, error) {
	cp := *s.c
	return &cp, nil
}

func (s *storedCampaign) Update(_ context.Context, c *models.Campaign, _ ...string) error {
	cp := *c
	s.c = &cp
	return nil
}

func (s *storedCampaign) SetDates(_ context.Context, _ string, oldStart, oldEnd, start, end *time.Time) (bool, error) {
	if !sameDay(s.c.StartDate, oldStart) || !sameDay(s.c.EndDate, oldEnd) {
		return false, nil
	}
	cp := *s.c
	cp.StartDate, cp.EndDate = start, end
	s.c = &cp
	return true, nil
}

func sameDay(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

type noPosts struct{ repository.PostRepository }

func (noPosts) ListByCampaign(context.Context, string) ([]models.Post, error) { return nil, nil }

// A date change logs what it replaced; undo puts it back.
func TestSetCampaignDates_IsUndoable(t *testing.T) {
	c := timelineCampaign()
	c.ID = "c1"
	campaigns := &storedCampaign{c: c}
	actions := &memActions{}
	st := &requestState{campaignID: "c1", campaign: c,
		repos: CampaignAssistantRepos{Campaigns: campaigns, Posts: noPosts{}, Actions: actions}}

	out := runTool(t, st, "setCampaignDates", map[string]any{"endDate": "2026-03-31"})
	if out.Status != "ok" || st.datesResult == nil || st.datesResult.UndoID == "" {
		t.Fatalf("outcome = %+v, result = %+v", out, st.datesResult)
	}
	if !campaigns.c.EndDate.Equal(day("2026-03-31")) {
		t.Fatalf("end date = %v, the change did not apply", campaigns.c.EndDate)
	}

	// The undo service (tested against Postgres) restores Before while the
	// campaign matches After.
	a := actions.logged(t, st.datesResult.UndoID)
	if a.Before["endDate"] != "2026-01-30T00:00:00Z" || a.After["endDate"] != "2026-03-31T00:00:00Z" {
		t.Fatalf("logged before = %v, after = %v", a.Before, a.After)
	}
	if want := assistantundo.CampaignDates(campaigns.c); !maps.Equal(a.After, want) {
		t.Fatalf("logged after = %v, want the stored campaign's %v", a.After, want)
	}
}

// Dates the user changed during the turn are left alone: the write is
// refused, so undo can never restore the turn's stale dates over them.
func TestSetCampaignDates_RefusedWhenChangedDuringTurn(t *testing.T) {
	c := timelineCampaign()
	c.ID = "c1"
	stored := *c
	stored.EndDate = new(day("2026-02-15"))
	campaigns := &storedCampaign{c: &stored}
	actions := &memActions{}
	st := &requestState{campaignID: "c1", campaign: c,
		repos: CampaignAssistantRepos{Campaigns: campaigns, Posts: noPosts{}, Actions: actions}}

	out := runTool(t, st, "setCampaignDates", map[string]any{"endDate": "2026-03-31"})
	if out.Status != "refused" || st.datesResult != nil || len(actions.rows) != 0 {
		t.Fatalf("outcome = %+v, result = %+v, logged = %d", out, st.datesResult, len(actions.rows))
	}
	if !campaigns.c.EndDate.Equal(day("2026-02-15")) {
		t.Fatalf("end date = %v, want the user's 2026-02-15", campaigns.c.EndDate)
	}
}

// An enriched brief logs all four fields it replaced, so undo can put them
// back.
func TestEnrichBrief_IsUndoable(t *testing.T) {
	c := timelineCampaign()
	c.ID = "c1"
	c.Description, c.TargetPersona, c.KeyMessages, c.ToneGuidelines = "desc", "persona", "messages", "tone"
	campaigns := &storedCampaign{c: c}
	actions := &memActions{}
	st := &requestState{campaignID: "c1", campaign: c,
		repos: CampaignAssistantRepos{Campaigns: campaigns, Actions: actions},
		enrichBrief: func(context.Context, enrich_brief.EnrichBriefRequest, enrich_brief.OnEventFunc) (*enrich_brief.EnrichBriefResponse, error) {
			return &enrich_brief.EnrichBriefResponse{Description: "new desc", TargetPersona: "new persona", KeyMessages: "new messages", ToneGuidelines: "new tone"}, nil
		}}

	runTool(t, st, "enrichBrief", map[string]any{"instruction": "sharper"})
	if st.briefResult == nil || st.briefResult.UndoID == "" || campaigns.c.Description != "new desc" {
		t.Fatalf("result = %+v, description = %q", st.briefResult, campaigns.c.Description)
	}
	a := actions.logged(t, st.briefResult.UndoID)
	want := models.JSONMap{"description": "desc", "targetPersona": "persona", "keyMessages": "messages", "toneGuidelines": "tone"}
	if !maps.Equal(a.Before, want) || a.After["toneGuidelines"] != "new tone" {
		t.Fatalf("logged before = %v, after = %v", a.Before, a.After)
	}
}

// Without an action log the change still applies; it just has no undo.
func TestSetCampaignDates_WithoutLogStillApplies(t *testing.T) {
	c := timelineCampaign()
	c.ID = "c1"
	campaigns := &storedCampaign{c: c}
	st := &requestState{campaignID: "c1", campaign: c, repos: CampaignAssistantRepos{Campaigns: campaigns, Posts: noPosts{}}}
	out := runTool(t, st, "setCampaignDates", map[string]any{"endDate": "2026-03-31"})
	if out.Status != "ok" || st.datesResult.UndoID != "" || !campaigns.c.EndDate.Equal(day("2026-03-31")) {
		t.Fatalf("outcome = %+v, result = %+v", out, st.datesResult)
	}
}
