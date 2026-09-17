package report

import (
	"testing"
	"time"
)

func mustLoad(t *testing.T, tz string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load %s: %v", tz, err)
	}
	return loc
}

// TestComputeReport_PopulatedDay checks every bucket aggregates, breaks down and
// orders deterministically for a day with activity.
func TestComputeReport_PopulatedDay(t *testing.T) {
	loc := mustLoad(t, "America/New_York")
	w, err := dayWindow("2026-08-18", loc)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	// A time inside the day, and one inside the previous day (must be excluded).
	in := time.Date(2026, 8, 18, 10, 0, 0, 0, loc)
	prev := time.Date(2026, 8, 17, 23, 0, 0, 0, loc)

	inputs := Inputs{
		Published: []PublishedRow{
			{At: in, PlatformID: "linkedin"},
			{At: in, PlatformID: "linkedin"},
			{At: in, PlatformID: "instagram"},
			{At: prev, PlatformID: "x"}, // excluded
		},
		Failed: []FailedRow{
			{At: in, PostID: "p_2", PlatformID: "x", Status: "failed", FailureReason: "zernio_rejected"},
			{At: in, PostID: "p_1", PlatformID: "x", Status: "not_published", FailureReason: ""},
		},
		Created: []CreatedRow{
			{At: in, CreatedBy: "ana", Scheduled: true},
			{At: in, CreatedBy: "ana", Scheduled: false},
			{At: in, CreatedBy: "bob", Scheduled: true},
		},
		Campaigns: []CampaignRow{
			{At: in, CampaignID: "c_2"},
			{At: in, CampaignID: "c_1"},
		},
	}

	r := computeReport(inputs, "2026-08-18", "America/New_York", w)

	if r.Published.Total != 3 {
		t.Fatalf("published total = %d, want 3", r.Published.Total)
	}
	// by_channel sorted by platform_id ascending.
	if len(r.Published.ByChannel) != 2 ||
		r.Published.ByChannel[0] != (ChannelCount{"instagram", 1}) ||
		r.Published.ByChannel[1] != (ChannelCount{"linkedin", 2}) {
		t.Fatalf("published by_channel wrong: %+v", r.Published.ByChannel)
	}
	if r.Failed.Total != 2 {
		t.Fatalf("failed total = %d, want 2", r.Failed.Total)
	}
	// posts sorted by post_id ascending.
	if len(r.Failed.Posts) != 2 || r.Failed.Posts[0].PostID != "p_1" || r.Failed.Posts[1].PostID != "p_2" {
		t.Fatalf("failed posts order/content wrong: %+v", r.Failed.Posts)
	}
	if r.Failed.Posts[0].Status != "not_published" || r.Failed.Posts[1].FailureReason != "zernio_rejected" {
		t.Fatalf("failed post detail wrong: %+v", r.Failed.Posts)
	}
	if r.Created.PostsTotal != 3 || r.Created.ScheduledTotal != 2 {
		t.Fatalf("created totals wrong: %+v", r.Created)
	}
	if len(r.Created.ByAuthor) != 2 || r.Created.ByAuthor[0] != (AuthorCount{"ana", 2}) || r.Created.ByAuthor[1] != (AuthorCount{"bob", 1}) {
		t.Fatalf("created by_author wrong: %+v", r.Created.ByAuthor)
	}
	if r.CampaignsCreated.Total != 2 {
		t.Fatalf("campaigns total = %d, want 2", r.CampaignsCreated.Total)
	}
	// campaign_ids sorted ascending.
	if len(r.CampaignsCreated.CampaignIDs) != 2 || r.CampaignsCreated.CampaignIDs[0] != "c_1" || r.CampaignsCreated.CampaignIDs[1] != "c_2" {
		t.Fatalf("campaign_ids wrong: %+v", r.CampaignsCreated.CampaignIDs)
	}
}

// TestComputeReport_EmptyDay returns a zeroed report with non-nil empty slices
// (so it serialises as [] not null) — a deep-link to a quiet day still renders.
func TestComputeReport_EmptyDay(t *testing.T) {
	loc := mustLoad(t, "UTC")
	w, _ := dayWindow("2026-01-01", loc)
	r := computeReport(Inputs{}, "2026-01-01", "UTC", w)

	if r.Published.Total != 0 || r.Failed.Total != 0 || r.Created.PostsTotal != 0 || r.CampaignsCreated.Total != 0 {
		t.Fatalf("expected all-zero report, got %+v", r)
	}
	if r.Published.ByChannel == nil || r.Failed.ByChannel == nil || r.Failed.Posts == nil ||
		r.Created.ByAuthor == nil || r.CampaignsCreated.CampaignIDs == nil {
		t.Fatalf("empty buckets must be non-nil slices: %+v", r)
	}
}

// TestComputeReport_CrossMidnightBoundary checks the half-open window puts each
// instant on the correct local day: 23:59:59 of the day counts, the next
// 00:00:00 does not (it belongs to the following day).
func TestComputeReport_CrossMidnightBoundary(t *testing.T) {
	loc := mustLoad(t, "America/New_York")
	w, _ := dayWindow("2026-08-18", loc)

	lastSecond := time.Date(2026, 8, 18, 23, 59, 59, 0, loc)
	nextMidnight := time.Date(2026, 8, 19, 0, 0, 0, 0, loc)

	r := computeReport(Inputs{Published: []PublishedRow{
		{At: lastSecond, PlatformID: "linkedin"},
		{At: nextMidnight, PlatformID: "linkedin"},
	}}, "2026-08-18", "America/New_York", w)

	if r.Published.Total != 1 {
		t.Fatalf("cross-midnight: want 1 published on the day, got %d", r.Published.Total)
	}
}

// TestDayWindow_DSTSpringForward checks a 23-hour local day is spanned by
// calendar arithmetic, not a fixed 24h. On 2026-03-08 US clocks jump 02:00→03:00,
// so the day is 23h; an instant at 22:30 local still falls inside the window.
func TestDayWindow_DSTSpringForward(t *testing.T) {
	loc := mustLoad(t, "America/New_York")
	w, err := dayWindow("2026-03-08", loc)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if got := w.Hi.Sub(w.Lo); got != 23*time.Hour {
		t.Fatalf("spring-forward day length = %v, want 23h", got)
	}
	late := time.Date(2026, 3, 8, 22, 30, 0, 0, loc)
	if !w.contains(late) {
		t.Fatalf("22:30 local should fall within the DST day window")
	}
	// The next day's midnight must be excluded.
	if w.contains(time.Date(2026, 3, 9, 0, 0, 0, 0, loc)) {
		t.Fatalf("next midnight must not be in the window")
	}
}

// TestDayWindow_DSTFallBack checks a 25-hour local day (clocks fall back
// 02:00→01:00 on 2026-11-01) is spanned correctly.
func TestDayWindow_DSTFallBack(t *testing.T) {
	loc := mustLoad(t, "America/New_York")
	w, _ := dayWindow("2026-11-01", loc)
	if got := w.Hi.Sub(w.Lo); got != 25*time.Hour {
		t.Fatalf("fall-back day length = %v, want 25h", got)
	}
}

// TestBucketReports_NonEmptyNewestFirst checks the list carries only days with
// activity, newest-first, capped at limit, with correct headline totals.
func TestBucketReports_NonEmptyNewestFirst(t *testing.T) {
	loc := mustLoad(t, "UTC")
	d18 := time.Date(2026, 8, 18, 12, 0, 0, 0, loc)
	d17 := time.Date(2026, 8, 17, 12, 0, 0, 0, loc)
	d15 := time.Date(2026, 8, 15, 12, 0, 0, 0, loc)
	// 2026-08-16 deliberately has no rows → must be absent.

	in := Inputs{
		Published: []PublishedRow{{At: d18, PlatformID: "x"}, {At: d17, PlatformID: "x"}},
		Failed:    []FailedRow{{At: d18, PostID: "p", PlatformID: "x", Status: "failed"}},
		Created:   []CreatedRow{{At: d15, CreatedBy: "ana"}},
		Campaigns: []CampaignRow{{At: d15, CampaignID: "c"}},
	}

	got := bucketReports(in, loc, 30, time.Time{})
	if len(got) != 3 {
		t.Fatalf("want 3 non-empty days, got %d (%+v)", len(got), got)
	}
	if got[0].Date != "2026-08-18" || got[1].Date != "2026-08-17" || got[2].Date != "2026-08-15" {
		t.Fatalf("days not newest-first / empty day leaked: %+v", got)
	}
	if got[0].PublishedTotal != 1 || got[0].FailedTotal != 1 {
		t.Fatalf("2026-08-18 totals wrong: %+v", got[0])
	}
	if got[2].CreatedTotal != 1 || got[2].CampaignsCreatedTotal != 1 {
		t.Fatalf("2026-08-15 totals wrong: %+v", got[2])
	}

	// limit caps the page to the newest days.
	if capped := bucketReports(in, loc, 2, time.Time{}); len(capped) != 2 || capped[0].Date != "2026-08-18" || capped[1].Date != "2026-08-17" {
		t.Fatalf("limit not applied newest-first: %+v", capped)
	}
}

// TestBucketReports_FloorDropsUndercoveredDays checks a truncation floor drops
// days that begin before it (possibly undercounted) while keeping fully-covered
// newer days.
func TestBucketReports_FloorDropsUndercoveredDays(t *testing.T) {
	loc := mustLoad(t, "UTC")
	d18 := time.Date(2026, 8, 18, 12, 0, 0, 0, loc)
	d15 := time.Date(2026, 8, 15, 12, 0, 0, 0, loc)
	in := Inputs{Published: []PublishedRow{{At: d18, PlatformID: "x"}, {At: d15, PlatformID: "x"}}}

	// Floor mid-way through 2026-08-16 → 08-15 (and older) dropped, 08-18 kept.
	floor := time.Date(2026, 8, 16, 6, 0, 0, 0, loc)
	got := bucketReports(in, loc, 30, floor)
	if len(got) != 1 || got[0].Date != "2026-08-18" {
		t.Fatalf("floor should keep only fully-covered days: %+v", got)
	}
}
