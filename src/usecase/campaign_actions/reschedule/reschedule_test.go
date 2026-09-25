package reschedule

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func ptrTime(t time.Time) *time.Time { return &t }

// 30-day campaign, 3 phases → 10-day windows:
// p1 [01-01,01-10], p2 [01-11,01-20], p3 [01-21,01-30].
func threePhaseCampaign() *models.Campaign {
	return &models.Campaign{
		StartDate: ptrTime(day("2026-01-01")),
		EndDate:   ptrTime(day("2026-01-30")),
		CampaignType: &models.CampaignType{Phases: []models.CampaignTypePhase{
			{ID: "p1", Sequence: 1},
			{ID: "p2", Sequence: 2},
			{ID: "p3", Sequence: 3},
		}},
	}
}

func mkPost(id, phase string, status models.PostStatus, created time.Time) models.Post {
	var ph *string
	if phase != "" {
		ph = &phase
	}
	return models.Post{ID: id, CampaignTypePhaseID: ph, Status: status, CreatedAt: created}
}

func planMap(c *models.Campaign, posts []models.Post) map[string]string {
	m := map[string]string{}
	for _, a := range Plan(c, posts) {
		m[a.PostID] = a.ScheduledAt.Format("2006-01-02")
	}
	return m
}

func TestPlan_PhaseAwareEvenSpread(t *testing.T) {
	base := day("2025-01-01")
	posts := []models.Post{
		mkPost("a1", "p1", models.PostStatusDraft, base),
		mkPost("a2", "p1", models.PostStatusReadyForPublish, base.AddDate(0, 0, 1)),
		mkPost("b1", "p2", models.PostStatusDraft, base),
		mkPost("c1", "p3", models.PostStatusDraft, base),
		mkPost("c2", "p3", models.PostStatusDraft, base.AddDate(0, 0, 1)),
		mkPost("c3", "p3", models.PostStatusDraft, base.AddDate(0, 0, 2)),
		mkPost("u1", "", models.PostStatusDraft, base), // unassigned → whole range
	}
	got := planMap(threePhaseCampaign(), posts)

	want := map[string]string{
		"a1": "2026-01-01", "a2": "2026-01-10", // 2 posts across p1 window
		"b1": "2026-01-11",                                         // single → window start
		"c1": "2026-01-21", "c2": "2026-01-25", "c3": "2026-01-30", // 3 across p3 window
		"u1": "2026-01-01", // unassigned → whole-range start
	}
	if len(got) != len(want) {
		t.Fatalf("assignment count = %d, want %d (%v)", len(got), len(want), got)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("post %s = %s, want %s", id, got[id], w)
		}
	}
}

func TestPlan_ExcludesCommittedAndPublished(t *testing.T) {
	posts := []models.Post{
		mkPost("draft", "p1", models.PostStatusDraft, day("2025-01-01")),
		mkPost("sched", "p1", models.PostStatusScheduled, day("2025-01-01")),
		mkPost("manual", "p1", models.PostStatusScheduledForManualPublish, day("2025-01-01")),
		mkPost("pub", "p1", models.PostStatusPublished, day("2025-01-01")),
		mkPost("failed", "p1", models.PostStatusFailed, day("2025-01-01")),
	}
	got := planMap(threePhaseCampaign(), posts)
	if len(got) != 1 {
		t.Fatalf("only the draft is eligible, got %v", got)
	}
	if _, ok := got["draft"]; !ok {
		t.Fatalf("draft should be redistributed, got %v", got)
	}
}

func TestPlan_NoDatesReturnsNil(t *testing.T) {
	c := threePhaseCampaign()
	c.StartDate, c.EndDate = nil, nil
	if got := Plan(c, []models.Post{mkPost("a", "p1", models.PostStatusDraft, day("2025-01-01"))}); got != nil {
		t.Fatalf("no dates should return nil, got %v", got)
	}
}

func TestPlan_StalePhaseTreatedAsUnassigned(t *testing.T) {
	posts := []models.Post{mkPost("x", "gone", models.PostStatusDraft, day("2025-01-01"))}
	got := planMap(threePhaseCampaign(), posts)
	// Unassigned bucket → whole-range start.
	if got["x"] != "2026-01-01" {
		t.Fatalf("stale-phase post = %s, want 2026-01-01", got["x"])
	}
}

func TestSpread_Evenness(t *testing.T) {
	ws, we := day("2026-02-01"), day("2026-02-11") // 10-day span
	got := spread(ws, we, 3)
	want := []string{"2026-02-01", "2026-02-06", "2026-02-11"}
	for i, w := range want {
		if got[i].Format("2006-01-02") != w {
			t.Errorf("spread[%d] = %s, want %s", i, got[i].Format("2006-01-02"), w)
		}
	}
	if s := spread(ws, we, 1); s[0] != ws {
		t.Fatalf("single spread = %v, want window start", s[0])
	}
}

// CON-166: a manual phase plan moves each phase's slice of the timeline.
func TestPlan_HonoursManualPhasePlan(t *testing.T) {
	c := threePhaseCampaign()
	c.PhaseWindows = []models.CampaignPhaseWindow{
		{PhaseID: "p1", StartDate: day("2026-01-01"), EndDate: day("2026-01-02")},
		{PhaseID: "p2", StartDate: day("2026-01-03"), EndDate: day("2026-01-28")},
		{PhaseID: "p3", StartDate: day("2026-01-29"), EndDate: day("2026-01-30")},
	}
	got := planMap(c, []models.Post{
		mkPost("b1", "p2", models.PostStatusDraft, day("2025-01-01")),
		mkPost("c1", "p3", models.PostStatusDraft, day("2025-01-01")),
	})
	if got["b1"] != "2026-01-03" || got["c1"] != "2026-01-29" {
		t.Fatalf("plan = %v, want p2 from 01-03 and p3 from 01-29", got)
	}
}
