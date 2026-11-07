package analytics

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/analytics/timeframe"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type platforms struct{ repository.PlatformRepository }

func (platforms) List(context.Context) ([]models.Platform, error) {
	return []models.Platform{{ID: "p-li", ZernioID: "linkedin"}, {ID: "p-x", ZernioID: "twitter"}}, nil
}

var errDBDown = errors.New("connection refused")

type campaigns struct{ repository.CampaignRepository }

func (campaigns) GetByID(_ context.Context, id string) (*models.Campaign, error) {
	switch id {
	case "c1":
		return &models.Campaign{ID: id}, nil
	case "c-down":
		return nil, errDBDown
	}
	return nil, sql.ErrNoRows
}

type scopeKeys struct{ repository.PostRepository }

func (scopeKeys) ScopeKeysByID(context.Context, []string) (map[string]repository.PostScopeKey, error) {
	return map[string]repository.PostScopeKey{
		"a": {PlatformID: "p-li", CampaignID: "c1"},
		"b": {PlatformID: "p-x", CampaignID: "c1"},
		"c": {PlatformID: "p-li", CampaignID: "c2"},
	}, nil
}

func TestScopeAndFilter(t *testing.T) {
	s := &Service{Platforms: platforms{}, Campaigns: campaigns{}, Posts: scopeKeys{}}
	ctx := t.Context()

	if sc, err := s.Scope(ctx, nil, ""); err != nil || sc != nil {
		t.Fatalf("no narrowing = %+v, %v; want the whole workspace", sc, err)
	}
	if _, err := s.Scope(ctx, []string{"myspace"}, ""); !errors.Is(err, ErrInvalidPlatform) {
		t.Fatalf("unknown slug: %v", err)
	}
	if _, err := s.Scope(ctx, nil, "c9"); !errors.Is(err, ErrCampaignNotFound) {
		t.Fatalf("unknown campaign: %v", err)
	}
	// A failed lookup is the failure, not a missing campaign.
	if _, err := s.Scope(ctx, nil, "c-down"); !errors.Is(err, errDBDown) || errors.Is(err, ErrCampaignNotFound) {
		t.Fatalf("lookup failure: %v", err)
	}
	sc, err := s.Scope(ctx, []string{" LinkedIn "}, "c1")
	if err != nil || !sc.AllowsSlug("linkedin") || sc.AllowsSlug("twitter") || sc.Campaign() != "c1" {
		t.Fatalf("scope = %+v, %v", sc, err)
	}
	rows := []models.PostAnalytics{{PostID: "a"}, {PostID: "b"}, {PostID: "c"}, {PostID: "unknown"}}
	if err := s.FilterRows(ctx, sc, &rows); err != nil || len(rows) != 1 || rows[0].PostID != "a" {
		t.Fatalf("filtered = %+v, %v", rows, err)
	}
}

func TestReads_WithoutAnalyticsDatabase(t *testing.T) {
	s := &Service{}
	if resp, reason, err := s.Overview(t.Context(), timeframe.Range{}, nil); resp != nil || reason != ReasonNotConfigured || err != nil {
		t.Fatalf("overview = %v, %q, %v", resp, reason, err)
	}
	if res, reason, err := s.Performers(t.Context(), timeframe.Range{}, nil, "reach", 5); res != nil || reason != ReasonNotConfigured || err != nil {
		t.Fatalf("performers = %v, %q, %v", res, reason, err)
	}
}
