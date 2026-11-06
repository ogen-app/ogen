package calendarcheck

import (
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

func at(s string) *time.Time {
	t, _ := time.Parse("2006-01-02T15:04", s)
	return &t
}

func campaign() *models.Campaign {
	start, end := at("2026-12-01T00:00"), at("2026-12-31T00:00")
	// Weekdays only.
	return &models.Campaign{ID: "c", StartDate: start, EndDate: end,
		PublishingDays: models.StringSlice{"mon", "tue", "wed", "thu", "fri"}}
}

func codes(fs []Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Code
	}
	return out
}

func find(fs []Finding, code string) *Finding {
	for i := range fs {
		if fs[i].Code == code {
			return &fs[i]
		}
	}
	return nil
}

func TestCheck(t *testing.T) {
	now := *at("2026-12-01T08:00")
	posts := []models.Post{
		// Outside the campaign, and movable.
		{ID: "late", Status: models.PostStatusDraft, PlatformID: "li", ScheduledAt: at("2027-01-04T09:00")},
		// A Saturday.
		{ID: "sat", Status: models.PostStatusReadyForPublish, PlatformID: "li", ScheduledAt: at("2026-12-05T09:00")},
		// Three LinkedIn posts on one day.
		{ID: "c1", Status: models.PostStatusReadyForPublish, PlatformID: "li", ScheduledAt: at("2026-12-08T09:00")},
		{ID: "c2", Status: models.PostStatusReadyForPublish, PlatformID: "li", ScheduledAt: at("2026-12-08T12:00")},
		{ID: "c3", Status: models.PostStatusScheduled, PlatformID: "li", ScheduledAt: at("2026-12-08T15:00")},
		// Due soon: unwritten, and not ready.
		{ID: "theses", Title: "Theses", Status: models.PostStatusPlanned, PlatformID: "x", ScheduledAt: at("2026-12-02T09:00")},
		{ID: "draft", Title: "Draft", Status: models.PostStatusDraft, PlatformID: "x", ScheduledAt: at("2026-12-03T09:00")},
		// Scheduled outside the window but committed: not movable, not flagged.
		{ID: "committed", Status: models.PostStatusScheduled, PlatformID: "x", ScheduledAt: at("2026-11-20T09:00")},
		// Published posts never count.
		{ID: "pub", Status: models.PostStatusPublished, PlatformID: "x", ScheduledAt: at("2026-12-12T09:00")},
	}
	got := Check(campaign(), posts, now)

	want := []string{CodeUnwrittenSoon, CodeNotReadySoon, CodeOutsideWindow, CodeDisabledDay, CodeCrowdedDay, CodeGap}
	if !slices.Equal(codes(got), want) {
		t.Fatalf("codes = %v, want %v", codes(got), want)
	}
	if f := find(got, CodeOutsideWindow); !slices.Equal(f.PostIDs, []string{"late"}) || f.Fixes[0].Kind != FixRedistribute {
		t.Fatalf("outside = %+v", f)
	}
	if f := find(got, CodeDisabledDay); !slices.Equal(f.PostIDs, []string{"sat"}) {
		t.Fatalf("disabled day = %+v", f)
	}
	if f := find(got, CodeCrowdedDay); !slices.Equal(f.PostIDs, []string{"c1", "c2", "c3"}) {
		t.Fatalf("crowded = %+v", f)
	}
	if f := find(got, CodeUnwrittenSoon); f.Fixes[0].Kind != FixOpenPost || f.Fixes[0].PostID != "theses" {
		t.Fatalf("unwritten = %+v", f)
	}
	// The silence after the 8th: the published post doesn't fill it.
	if f := find(got, CodeGap); f.Severity != SeverityInfo || f.Fixes[0].From != "2026-12-09" || f.Fixes[0].To != "2026-12-31" {
		t.Fatalf("gap = %+v", f)
	}

	// Keys are stable for the same posts, and differ with them.
	again := Check(campaign(), posts, now)
	if again[0].Key != got[0].Key {
		t.Fatal("the same finding got another key")
	}
	if find(got, CodeCrowdedDay).Key == find(got, CodeOutsideWindow).Key {
		t.Fatal("two findings share a key")
	}
}

func TestCheck_UndatedCampaign(t *testing.T) {
	c := &models.Campaign{ID: "c"}
	got := Check(c, []models.Post{{ID: "p", Status: models.PostStatusDraft, ScheduledAt: at("2026-12-05T09:00")}}, *at("2026-01-01T00:00"))
	if len(got) != 0 {
		t.Fatalf("an undated campaign with every day enabled = %v", codes(got))
	}
}
