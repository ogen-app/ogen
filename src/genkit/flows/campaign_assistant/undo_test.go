package campaign_assistant

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
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

type noPosts struct{ repository.PostRepository }

func (noPosts) ListByCampaign(context.Context, string) ([]models.Post, error) { return nil, nil }

// A date change logs what it replaced, for undo to put back.
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
	a, err := actions.GetByID(t.Context(), st.datesResult.UndoID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Before["endDate"] != "2026-01-30T00:00:00Z" || a.After["endDate"] != "2026-03-31T00:00:00Z" {
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
