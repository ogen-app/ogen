package campaignphase

import (
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func ptr(t time.Time) *time.Time { return &t }

// campaign builds a 3-phase campaign over [start, end]; phases are declared
// out of sequence order to prove everything sorts by Sequence.
func campaign(start, end string) *models.Campaign {
	c := &models.Campaign{
		ID: "c1",
		CampaignType: &models.CampaignType{Phases: []models.CampaignTypePhase{
			{ID: "p3", Name: "Three", Sequence: 3},
			{ID: "p1", Name: "One", Sequence: 1},
			{ID: "p2", Name: "Two", Sequence: 2},
		}},
	}
	if start != "" {
		c.StartDate = ptr(day(start))
	}
	if end != "" {
		c.EndDate = ptr(day(end))
	}
	return c
}

func spans(ws []Window) [][3]string {
	out := make([][3]string, len(ws))
	for i, w := range ws {
		out[i] = [3]string{w.Phase.ID, w.Start.Format(time.DateOnly), w.End.Format(time.DateOnly)}
	}
	return out
}

func equal(a, b [][3]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSplit_RemainderToEarliest(t *testing.T) {
	// 10 days / 3 → 4,3,3.
	got := Split(day("2026-01-01"), day("2026-01-10"), 3)
	want := [][2]string{{"2026-01-01", "2026-01-04"}, {"2026-01-05", "2026-01-07"}, {"2026-01-08", "2026-01-10"}}
	for i, w := range want {
		if got[i][0].Format(time.DateOnly) != w[0] || got[i][1].Format(time.DateOnly) != w[1] {
			t.Fatalf("window %d = %v, want %v", i, got[i], w)
		}
	}
}

func TestSplit_FewerDaysThanPhasesCollapsesToFullRange(t *testing.T) {
	got := Split(day("2026-01-01"), day("2026-01-02"), 3)
	for i, w := range got {
		if !w[0].Equal(day("2026-01-01")) || !w[1].Equal(day("2026-01-02")) {
			t.Fatalf("window %d = %v, want the full range", i, w)
		}
	}
	if Split(day("2026-01-01"), day("2026-01-02"), 0) != nil {
		t.Fatal("n=0 must yield nil")
	}
}

func TestResolve_DerivedWhenNoStoredPlan(t *testing.T) {
	c := campaign("2026-01-01", "2026-01-10")
	ws, src := Resolve(c)
	if src != SourceDerived {
		t.Fatalf("source = %s, want derived", src)
	}
	want := [][3]string{{"p1", "2026-01-01", "2026-01-04"}, {"p2", "2026-01-05", "2026-01-07"}, {"p3", "2026-01-08", "2026-01-10"}}
	if !equal(spans(ws), want) {
		t.Fatalf("windows = %v, want %v", spans(ws), want)
	}
}

func TestResolve_Unscheduled(t *testing.T) {
	ws, src := Resolve(campaign("2026-01-01", ""))
	if src != SourceUnscheduled || ws != nil {
		t.Fatalf("got %v %s, want nil unscheduled", ws, src)
	}
}

func stored(rows ...[3]string) []models.CampaignPhaseWindow {
	out := make([]models.CampaignPhaseWindow, len(rows))
	for i, r := range rows {
		out[i] = models.CampaignPhaseWindow{PhaseID: r[0], StartDate: day(r[1]), EndDate: day(r[2])}
	}
	return out
}

func TestResolve_ManualPlanWins(t *testing.T) {
	c := campaign("2026-01-01", "2026-01-10")
	c.PhaseWindows = stored(
		[3]string{"p2", "2026-01-03", "2026-01-08"}, // out of order on purpose
		[3]string{"p1", "2026-01-01", "2026-01-02"},
		[3]string{"p3", "2026-01-09", "2026-01-10"},
	)
	ws, src := Resolve(c)
	if src != SourceManual {
		t.Fatalf("source = %s, want manual", src)
	}
	want := [][3]string{{"p1", "2026-01-01", "2026-01-02"}, {"p2", "2026-01-03", "2026-01-08"}, {"p3", "2026-01-09", "2026-01-10"}}
	if !equal(spans(ws), want) {
		t.Fatalf("windows = %v, want %v", spans(ws), want)
	}
}

func TestResolve_StalePlanFallsBackToDerived(t *testing.T) {
	c := campaign("2026-01-01", "2026-01-10")
	// Anchored to old dates (campaign now starts Jan 1, plan says Dec 30).
	c.PhaseWindows = stored(
		[3]string{"p1", "2025-12-30", "2026-01-02"},
		[3]string{"p2", "2026-01-03", "2026-01-08"},
		[3]string{"p3", "2026-01-09", "2026-01-10"},
	)
	if _, src := Resolve(c); src != SourceDerived {
		t.Fatalf("source = %s, want derived for a stale plan", src)
	}
	// A plan for a phase set that no longer matches (p3 missing).
	c.PhaseWindows = stored([3]string{"p1", "2026-01-01", "2026-01-05"}, [3]string{"p2", "2026-01-06", "2026-01-10"})
	if _, src := Resolve(c); src != SourceDerived {
		t.Fatalf("source = %s, want derived for a mismatched phase set", src)
	}
}

func TestValidate_Rules(t *testing.T) {
	c := campaign("2026-01-01", "2026-01-10")
	e := func(id, s, en string) Entry { return Entry{PhaseID: id, Start: day(s), End: day(en)} }
	valid := []Entry{e("p1", "2026-01-01", "2026-01-02"), e("p2", "2026-01-03", "2026-01-08"), e("p3", "2026-01-09", "2026-01-10")}
	if _, err := Validate(c, valid); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}

	cases := map[string][]Entry{
		"gap":           {e("p1", "2026-01-01", "2026-01-02"), e("p2", "2026-01-04", "2026-01-08"), e("p3", "2026-01-09", "2026-01-10")},
		"overlap":       {e("p1", "2026-01-01", "2026-01-03"), e("p2", "2026-01-03", "2026-01-08"), e("p3", "2026-01-09", "2026-01-10")},
		"late start":    {e("p1", "2026-01-02", "2026-01-02"), e("p2", "2026-01-03", "2026-01-08"), e("p3", "2026-01-09", "2026-01-10")},
		"early end":     {e("p1", "2026-01-01", "2026-01-02"), e("p2", "2026-01-03", "2026-01-08"), e("p3", "2026-01-09", "2026-01-09")},
		"inverted":      {e("p1", "2026-01-01", "2026-01-02"), e("p2", "2026-01-08", "2026-01-03"), e("p3", "2026-01-09", "2026-01-10")},
		"missing phase": {e("p1", "2026-01-01", "2026-01-05"), e("p2", "2026-01-06", "2026-01-10")},
		"foreign phase": {e("p1", "2026-01-01", "2026-01-02"), e("p2", "2026-01-03", "2026-01-08"), e("p3", "2026-01-09", "2026-01-10"), e("x", "2026-01-01", "2026-01-01")},
		"duplicate":     {e("p1", "2026-01-01", "2026-01-02"), e("p1", "2026-01-01", "2026-01-02"), e("p2", "2026-01-03", "2026-01-08"), e("p3", "2026-01-09", "2026-01-10")},
	}
	for name, plan := range cases {
		_, err := Validate(c, plan)
		var pe *PlanError
		if !errors.As(err, &pe) {
			t.Errorf("%s: err = %v, want *PlanError", name, err)
		}
	}

	if _, err := Validate(campaign("", ""), valid); !errors.Is(err, ErrUnscheduled) {
		t.Errorf("unscheduled: err = %v, want ErrUnscheduled", err)
	}
	// Too short to split: 2 days can't give 3 phases ≥1 day each.
	short := campaign("2026-01-01", "2026-01-02")
	if _, err := Validate(short, []Entry{e("p1", "2026-01-01", "2026-01-01"), e("p2", "2026-01-02", "2026-01-02"), e("p3", "2026-01-03", "2026-01-02")}); err == nil {
		t.Error("too-short campaign: want an error")
	}
}

func TestPhaseAt(t *testing.T) {
	c := campaign("2026-01-01", "2026-01-10") // derived: p1 1–4, p2 5–7, p3 8–10
	cases := map[string]string{
		"2025-12-01T10:00:00Z": "p1", // before start
		"2026-01-04T23:00:00Z": "p1", // last day of p1, late in the day
		"2026-01-05T00:00:00Z": "p2",
		"2026-01-10T12:00:00Z": "p3",
		"2026-02-01T00:00:00Z": "p3", // after end
	}
	for at, want := range cases {
		tt, _ := time.Parse(time.RFC3339, at)
		if got, _ := PhaseAt(c, tt); got.ID != want {
			t.Errorf("PhaseAt(%s) = %s, want %s", at, got.ID, want)
		}
	}
	// Manual plan moves the boundary.
	c.PhaseWindows = stored(
		[3]string{"p1", "2026-01-01", "2026-01-01"},
		[3]string{"p2", "2026-01-02", "2026-01-09"},
		[3]string{"p3", "2026-01-10", "2026-01-10"},
	)
	if got, _ := PhaseAt(c, day("2026-01-04")); got.ID != "p2" {
		t.Errorf("manual: PhaseAt(Jan 4) = %s, want p2", got.ID)
	}
	if got, _ := PhaseAt(campaign("", ""), day("2026-01-04")); got.ID != "p1" {
		t.Errorf("undated: PhaseAt = %s, want the first phase", got.ID)
	}
	if _, ok := PhaseAt(&models.Campaign{}, day("2026-01-04")); ok {
		t.Error("no phases: want ok=false")
	}
}

func TestRebound(t *testing.T) {
	plan := stored(
		[3]string{"p1", "2026-01-01", "2026-01-02"},
		[3]string{"p2", "2026-01-03", "2026-01-08"},
		[3]string{"p3", "2026-01-09", "2026-01-10"},
	)
	// Extending both ends keeps the inner boundaries.
	got, ok := Rebound(campaign("2025-12-28", "2026-01-20"), plan)
	if !ok {
		t.Fatal("extension should keep the plan")
	}
	if got[0].Start.Format(time.DateOnly) != "2025-12-28" || got[2].End.Format(time.DateOnly) != "2026-01-20" {
		t.Fatalf("rebounded = %+v", got)
	}
	if got[1].Start.Format(time.DateOnly) != "2026-01-03" || got[1].End.Format(time.DateOnly) != "2026-01-08" {
		t.Fatalf("inner window moved: %+v", got[1])
	}
	// Shrinking past the first window's end empties it → drop.
	if _, ok := Rebound(campaign("2026-01-03", "2026-01-10"), plan); ok {
		t.Fatal("a start past the first window must drop the plan")
	}
	// Losing the dates drops it too.
	if _, ok := Rebound(campaign("", ""), plan); ok {
		t.Fatal("an undated campaign must drop the plan")
	}
}
