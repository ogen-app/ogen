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

// Slots are the open dated ideas in range: not turned down, not yet a post,
// not in an archived campaign unless asked for by campaign.
func TestIdeas_Slots(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewIdeaRepository(db)
	links := repository.NewPostIdeaRepository(db)
	ctx := tenantctx.With(t.Context(), "tenant-idea-slots")

	now := time.Now().UTC()
	for _, c := range []*models.Campaign{
		{ID: "slot-live", Name: "Live"},
		{ID: "slot-archived", Name: "Old", ArchivedAt: &now},
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
	no, yes := models.IdeaVerdictNo, models.IdeaVerdictYes
	live, archived := "slot-live", "slot-archived"
	for _, i := range []*models.Idea{
		{ID: "s-late", Title: "late", CampaignID: &live, Plan: models.IdeaPlan{TargetDate: day("2026-12-20"), PlatformID: new("linkedin")}},
		{ID: "s-early", Title: "early", CampaignID: &live, Verdict: &yes, DecidedAt: &now, DecidedBy: new("u1"), Plan: models.IdeaPlan{TargetDate: day("2026-12-02")}},
		{ID: "s-workspace", Title: "workspace", Plan: models.IdeaPlan{TargetDate: day("2026-12-05")}},
		{ID: "s-no", Title: "turned down", CampaignID: &live, Verdict: &no, DecidedAt: &now, DecidedBy: new("u1"), Plan: models.IdeaPlan{TargetDate: day("2026-12-03")}},
		{ID: "s-posted", Title: "made into a post", CampaignID: &live, Plan: models.IdeaPlan{TargetDate: day("2026-12-04")}},
		{ID: "s-archived", Title: "archived campaign", CampaignID: &archived, Plan: models.IdeaPlan{TargetDate: day("2026-12-06")}},
		{ID: "s-january", Title: "out of range", CampaignID: &live, Plan: models.IdeaPlan{TargetDate: day("2027-01-02")}},
		{ID: "s-undated", Title: "no date", CampaignID: &live},
	} {
		if err := repo.Create(ctx, i); err != nil {
			t.Fatalf("create %s: %v", i.ID, err)
		}
	}
	if err := links.Link(ctx, db, "post-from-idea", []string{"s-posted"}); err != nil {
		t.Fatal(err)
	}

	ids := func(slots []models.IdeaSlot) []string {
		out := make([]string, len(slots))
		for i, s := range slots {
			out[i] = s.ID
		}
		return out
	}
	from, to := *day("2026-12-01"), *day("2026-12-31")

	all, err := repo.Slots(ctx, from, to, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(all); !slices.Equal(got, []string{"s-early", "s-workspace", "s-late"}) {
		t.Fatalf("workspace slots = %v", got)
	}
	late := all[2]
	if late.Kind != "idea" || late.Date.String() != "2026-12-20" || late.CampaignName != "Live" || *late.PlatformID != "linkedin" || late.Verdict != nil {
		t.Fatalf("slot = %+v", late)
	}

	// Asked for by campaign, an archived campaign's ideas are its slots.
	if got, err := repo.Slots(ctx, from, to, "slot-archived"); err != nil || !slices.Equal(ids(got), []string{"s-archived"}) {
		t.Fatalf("archived campaign slots = %v (%v)", ids(got), err)
	}
	if got, err := repo.Slots(ctx, from, to, "slot-live"); err != nil || !slices.Equal(ids(got), []string{"s-early", "s-late"}) {
		t.Fatalf("live campaign slots = %v (%v)", ids(got), err)
	}

	other := tenantctx.With(context.Background(), "tenant-other")
	if got, err := repo.Slots(other, from, to, ""); err != nil || len(got) != 0 {
		t.Fatalf("another tenant read %v (%v)", ids(got), err)
	}
}
