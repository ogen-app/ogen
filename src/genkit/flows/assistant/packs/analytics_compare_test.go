package packs

import (
	"testing"

	"github.com/ogen-app/ogen/src/analytics/overview"
	"github.com/ogen-app/ogen/src/usecase/analytics"
)

// Two periods' cards pair by metric, with the change worked out; a metric
// at zero in the first has no percentage.
func TestCompareCards(t *testing.T) {
	got := compareCards(
		[]overview.Card{{Metric: "reach", Label: "Cumulative reach", Value: 1000}, {Metric: "posts_published", Value: 0}, {Metric: "followers", Value: 50}, {Metric: "engagement_rate", Value: 3.6}},
		[]overview.Card{{Metric: "followers", Value: 60}, {Metric: "reach", Value: 1250}, {Metric: "posts_published", Value: 4}, {Metric: "engagement_rate", Value: 4.2}},
	)
	if len(got) != 4 || got[0].Metric != "reach" || got[0].Unit != UnitCount || got[0].Change != 250 || got[0].ChangePct == nil || *got[0].ChangePct != 25 {
		t.Fatalf("reach = %+v", got)
	}
	if got[1].ChangePct != nil || got[1].Change != 4 {
		t.Fatalf("posts = %+v", got[1])
	}
	if *got[2].ChangePct != 20 {
		t.Fatalf("followers = %+v", got[2])
	}
	// A rate's change is in points.
	if got[3].Unit != UnitRate || got[3].Change != 0.6 {
		t.Fatalf("rate = %+v", got[3])
	}
}

// Both periods are cited with the one scope; without the analytics
// database there are no figures, and says why. One period alone is refused.
func TestComparePeriods(t *testing.T) {
	d := Deps{Analytics: &analytics.Service{Platforms: slugPlatforms{}}}
	ctx := analyticsTurn(t, nil)
	out, err := comparePeriods(ctx, d, ComparePeriodsInput{
		First: Period{From: "2026-03-01", To: "2026-03-31"}, Second: Period{From: "2026-04-01", To: "2026-04-30"}, Platforms: []string{"linkedin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.First.From != "2026-03-01" || out.Second.To != "2026-04-30" || out.Second.Platforms[0] != "linkedin" || out.Unavailable != analytics.ReasonNotConfigured {
		t.Fatalf("out = %+v", out)
	}
	_, err = comparePeriods(ctx, d, ComparePeriodsInput{First: Period{Window: "28d"}})
	refused(t, err)
}
