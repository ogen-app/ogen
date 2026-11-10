package repository_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Planned ideas are the open ones with a plan, the soonest first and the
// undated last: not turned down, not yet a post, not in an archived
// campaign unless asked for by campaign, and only the caller's.
func TestIdeas_PlannedIdeas(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewIdeaRepository(db)
	links := repository.NewPostIdeaRepository(db)
	ctx := tenantctx.With(t.Context(), "tenant-idea-planned")

	now := time.Now().UTC()
	for _, c := range []*models.Campaign{
		{ID: "plan-live", Name: "Live"},
		{ID: "plan-archived", Name: "Old", ArchivedAt: &now},
	} {
		c.CampaignTypeID, c.CreatedBy = "t", "u1"
		c.AssetIDs, c.PublishingDays, c.TargetPlatforms = models.StringSlice{}, models.StringSlice{}, models.CampaignPlatforms{}
		if _, err := db.NewInsert().Model(c).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	day := func(s string) *models.CalendarDate {
		d, err := models.ParseCalendarDate(s)
		if err != nil {
			t.Fatal(err)
		}
		return &d
	}
	no := models.IdeaVerdictNo
	live, archived := "plan-live", "plan-archived"
	for _, i := range []*models.Idea{
		{ID: "p-late", Title: "late", CampaignID: &live, Plan: models.IdeaPlan{TargetDate: day("2026-12-20")}},
		{ID: "p-early", Title: "early", CampaignID: &live, Plan: models.IdeaPlan{TargetDate: day("2026-12-02")}},
		{ID: "p-undated", Title: "platform only", Plan: models.IdeaPlan{PlatformID: new("linkedin")}},
		{ID: "p-none", Title: "no plan", CampaignID: &live},
		{ID: "p-no", Title: "turned down", CampaignID: &live, Verdict: &no, DecidedAt: &now, DecidedBy: new("u1"), Plan: models.IdeaPlan{TargetDate: day("2026-12-03")}},
		{ID: "p-posted", Title: "made into a post", CampaignID: &live, Plan: models.IdeaPlan{TargetDate: day("2026-12-04")}},
		{ID: "p-archived", Title: "archived campaign", CampaignID: &archived, Plan: models.IdeaPlan{TargetDate: day("2026-12-05")}},
	} {
		if err := repo.Create(ctx, i); err != nil {
			t.Fatalf("create %s: %v", i.ID, err)
		}
	}
	if err := links.Link(ctx, db, "post-planned", []string{"p-posted"}); err != nil {
		t.Fatal(err)
	}
	ids := func(list []models.Idea) []string {
		var out []string
		for _, i := range list {
			out = append(out, i.ID)
		}
		return out
	}

	got, total, err := repo.PlannedIdeas(ctx, "", 10)
	if err != nil || total != 3 || !slices.Equal(ids(got), []string{"p-early", "p-late", "p-undated"}) {
		t.Fatalf("workspace = %v, %d, %v", ids(got), total, err)
	}
	if got, total, _ := repo.PlannedIdeas(ctx, "", 1); total != 3 || !slices.Equal(ids(got), []string{"p-early"}) {
		t.Fatalf("capped = %v, %d", ids(got), total)
	}
	if got, total, _ := repo.PlannedIdeas(ctx, archived, 10); total != 1 || !slices.Equal(ids(got), []string{"p-archived"}) {
		t.Fatalf("archived campaign = %v, %d", ids(got), total)
	}
	other := tenantctx.With(context.Background(), "tenant-other-planned")
	if got, total, _ := repo.PlannedIdeas(other, "", 10); len(got) != 0 || total != 0 {
		t.Fatalf("another tenant read %v (%d)", ids(got), total)
	}
}
