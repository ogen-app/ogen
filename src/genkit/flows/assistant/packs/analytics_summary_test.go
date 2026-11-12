package packs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/analytics"
)

type twoPlatforms struct {
	repository.SocialAccountRepository
}

func (twoPlatforms) ListActive(context.Context, string) ([]models.SocialAccount, error) {
	return []models.SocialAccount{{Platform: "linkedin"}, {Platform: "Instagram"}, {Platform: "linkedin"}}, nil
}

// zernioInsights answers best-time, and content-decay per platform:
// Instagram reaches half sooner, and LinkedIn's read fails.
func zernioInsights(t *testing.T) *zernio.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/analytics/best-time" {
			_, _ = w.Write([]byte(`{"slots":[{"day_of_week":4,"hour":18,"avg_engagement":50,"post_count":9}]}`))
			return
		}
		switch r.URL.Query().Get("platform") {
		case "":
			_, _ = w.Write([]byte(`{"buckets":[{"bucket_order":0,"bucket_label":"0-6h","avg_pct_of_final":40},{"bucket_order":1,"bucket_label":"6-12h","avg_pct_of_final":60}]}`))
		case "instagram":
			_, _ = w.Write([]byte(`{"buckets":[{"bucket_order":0,"bucket_label":"0-6h","avg_pct_of_final":60}]}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return zernio.NewClient(zernio.StaticKey("k"), srv.URL, zernio.ClientOpts{Timeout: 5 * time.Second})
}

func insightsDeps(t *testing.T) Deps {
	return Deps{Analytics: &analytics.Service{Platforms: slugPlatforms{}, Accounts: twoPlatforms{}, Zernio: zernioInsights(t),
		ProfileID: func(context.Context) (string, error) { return "prof", nil }}}
}

// Best times rank the slots beside the grid.
func TestBestTimesRead_Summary(t *testing.T) {
	out, err := bestTimesRead(analyticsTurn(t, nil), insightsDeps(t), InsightInput{})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := out.Summary.(analytics.BestTimesSummary)
	if !ok || s.Best == nil || s.Best.DayOfWeek != 4 || s.Unit != analytics.UnitAvgInteractions {
		t.Fatalf("summary = %#v", out.Summary)
	}
	if slots, ok := out.Data.([]zernio.BestTimeSlot); !ok || len(slots) != 1 {
		t.Fatalf("data = %#v", out.Data)
	}
}

// Decay says its half-life, and each connected platform's when it spans
// several; a platform the publisher fails on is left out.
func TestDecayRead_PerPlatform(t *testing.T) {
	d := insightsDeps(t)
	out, err := decayRead(analyticsTurn(t, nil), d, InsightInput{})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := out.Summary.(*DecaySummary)
	if !ok || s.HalfLife == nil || s.HalfLife.Hours != 9 {
		t.Fatalf("summary = %#v", out.Summary)
	}
	if len(s.Platforms) != 1 || s.Platforms[0].Platform != "instagram" || s.Platforms[0].HalfLife.Hours != 5 {
		t.Fatalf("platforms = %+v", s.Platforms)
	}

	// Narrowed to one platform, it doesn't split.
	out, err = decayRead(analyticsTurn(t, &viewcontext.Window{Platforms: []string{"instagram"}}), d, InsightInput{})
	if s, ok := out.Summary.(*DecaySummary); err != nil || !ok || s.HalfLife.Hours != 5 || s.Platforms != nil {
		t.Fatalf("narrowed = %+v, %v", out.Summary, err)
	}
}

type publishedPosts struct {
	repository.PostAnalyticsRepository
	rows []models.PostAnalytics
}

func (p *publishedPosts) PublishedBetween(context.Context, time.Time, time.Time) ([]models.PostAnalytics, error) {
	return p.rows, nil
}

// Frequency carries the cadence the workspace keeps, on the screen's
// platform, beside the publisher's rows.
func TestFrequencyRead_Cadence(t *testing.T) {
	repo := &publishedPosts{rows: []models.PostAnalytics{
		{Platform: "LinkedIn", PublishedAt: new(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))},
		{Platform: "Instagram", PublishedAt: new(time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC))},
	}}
	d := Deps{Analytics: &analytics.Service{Repo: repo, Platforms: namedPlatforms{}}}
	ctx := analyticsTurn(t, &viewcontext.Window{Platforms: []string{"linkedin"}})
	out, err := frequencyRead(ctx, d, InsightInput{}, time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := out.Summary.(*FrequencySummary)
	if !ok || s.Note == "" || len(s.Cadence.Platforms) != 1 || s.Cadence.Platforms[0].Platform != "linkedin" ||
		s.Cadence.Weeks[len(s.Cadence.Weeks)-2].Posts != 1 {
		t.Fatalf("summary = %#v", out.Summary)
	}
	if u, ok := out.Data.(Unavailable); !ok || u.Reason != analytics.ReasonNotConfigured {
		t.Fatalf("data = %#v", out.Data)
	}
}
