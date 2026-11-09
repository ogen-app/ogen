package overview

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/calendarcheck"
)

type oneCampaign struct {
	repository.CampaignRepository
	c *models.Campaign
}

func (r oneCampaign) GetByID(_ context.Context, id string) (*models.Campaign, error) {
	if id != r.c.ID {
		return nil, sql.ErrNoRows
	}
	return r.c, nil
}

type rows struct {
	repository.PostRepository
	posts []models.Post
}

func (r rows) ListRowsByCampaign(context.Context, string, ...string) ([]models.Post, error) {
	return r.posts, nil
}

type noPlatforms struct{ repository.PlatformRepository }

func (noPlatforms) List(context.Context) ([]models.Platform, error) { return nil, nil }

type dismissals struct {
	repository.AssistantDismissalRepository
	keys map[string]bool
}

func (d *dismissals) Keys(_ context.Context, kind models.AssistantScopeKind, id string) (map[string]bool, error) {
	if kind != models.AssistantScopeCampaign || id != "camp-1" {
		return nil, errors.New("wrong thread")
	}
	return d.keys, nil
}

func (d *dismissals) Dismiss(_ context.Context, _ models.AssistantScopeKind, _, key, _ string) error {
	d.keys[key] = true
	return nil
}

// The overview carries the calendar's problems with their fixes; one
// dismissed from the overview is left out after, and counted.
func TestOverview_Problems(t *testing.T) {
	c := sampleCampaign()
	start, end := dateUTC(2099, 3, 1), dateUTC(2099, 3, 31)
	c.StartDate, c.EndDate = &start, &end
	late := dateUTC(2099, 4, 6)
	posts := []models.Post{{ID: "late", Title: "Late", Status: models.PostStatusDraft, PlatformID: "li", ScheduledAt: &late}}
	d := &dismissals{keys: map[string]bool{}}
	svc := New(oneCampaign{c: c}, rows{posts: posts}, noPlatforms{}).WithProblems(nil, d)

	ov, err := svc.Overview(t.Context(), "camp-1")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ov.Problems, func(f calendarcheck.Finding) bool { return f.Code == "outside_window" })
	if i < 0 || len(ov.Problems[i].Fixes) == 0 {
		t.Fatalf("problems = %+v", ov.Problems)
	}
	if err := svc.Dismiss(t.Context(), "camp-1", ov.Problems[i].Key, "u1"); err != nil {
		t.Fatal(err)
	}
	again, err := svc.Overview(t.Context(), "camp-1")
	if err != nil {
		t.Fatal(err)
	}
	if again.ProblemsDismissed != 1 || len(again.Problems) != len(ov.Problems)-1 {
		t.Fatalf("after dismissing: %d problems, %d dismissed", len(again.Problems), again.ProblemsDismissed)
	}
	if err := svc.Dismiss(t.Context(), "nope", "k", "u1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown campaign = %v", err)
	}

	// Without problems the overview has none, and nothing to dismiss.
	plain := New(oneCampaign{c: c}, rows{posts: posts}, noPlatforms{})
	if ov, _ := plain.Overview(t.Context(), "camp-1"); ov.Problems == nil || len(ov.Problems) != 0 {
		t.Fatalf("plain problems = %v", ov.Problems)
	}
	if err := plain.Dismiss(t.Context(), "camp-1", "k", "u1"); !errors.Is(err, ErrNoProblems) {
		t.Fatalf("plain dismiss = %v", err)
	}
}
