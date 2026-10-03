package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// insightFakeFollowerRepo is an in-memory FollowerStatsRepository for the
// handler tests (no DB needed — the endpoints don't touch one on read).
type insightFakeFollowerRepo struct {
	summary []repository.FollowerAccountSummary
	series  []repository.FollowerSeriesPoint
}

func (r *insightFakeFollowerRepo) InsertMany(context.Context, []models.FollowerSnapshot) (int, error) {
	return 0, nil
}
func (r *insightFakeFollowerRepo) Summary(context.Context, string) ([]repository.FollowerAccountSummary, error) {
	return r.summary, nil
}
func (r *insightFakeFollowerRepo) Series(context.Context, repository.FollowerSeriesOptions) ([]repository.FollowerSeriesPoint, error) {
	return r.series, nil
}
func (r *insightFakeFollowerRepo) LatestPointDate(context.Context) (time.Time, error) {
	return time.Time{}, nil
}

func insightNoAuth(c *fiber.Ctx) error { return c.Next() }

// insightApp builds a fiber app with just the AnalyticsHandler, a Zernio client
// pointed at handler, and the given profile resolver + follower repo.
func insightApp(handler http.HandlerFunc, profileID func(context.Context) (string, error), followerRepo repository.FollowerStatsRepository) (*fiber.App, *httptest.Server) {
	stub := httptest.NewServer(handler)
	client := zernio.NewClient(zernio.StaticKey("k"), stub.URL, zernio.ClientOpts{Timeout: 5 * time.Second})
	app := fiber.New()
	handlers.NewAnalyticsHandler(nil, followerRepo, nil, nil, nil, nil, client, profileID, insightNoAuth).Register(app)
	return app, stub
}

func doGet(t *testing.T, app *fiber.App, path string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequestWithContext(t.Context(), "GET", path, nil))
	if err != nil {
		t.Fatalf("request %s: %v", path, err)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	_ = resp.Body.Close()
	return resp, body
}

func okProfile(context.Context) (string, error) { return "prof-1", nil }

func TestBestTimesAvailable(t *testing.T) {
	app, stub := insightApp(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/analytics/best-time" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("profileId"); got != "prof-1" {
			t.Errorf("profileId: got %q want prof-1", got)
		}
		_, _ = w.Write([]byte(`{"slots":[{"day_of_week":2,"hour":18,"avg_engagement":510.3,"post_count":15}]}`))
	}, okProfile, nil)
	defer stub.Close()

	resp, body := doGet(t, app, "/api/analytics/best-times?platform=instagram")
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if body["available"] != true {
		t.Fatalf("available: got %v want true (%v)", body["available"], body)
	}
	data, _ := body["data"].(map[string]any)
	slots, _ := data["slots"].([]any)
	if len(slots) != 1 {
		t.Fatalf("slots: got %v", data["slots"])
	}
}

func TestInsightAddonRequiredIsGraceful(t *testing.T) {
	app, stub := insightApp(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"Analytics add-on required","requiresAddon":true}`))
	}, okProfile, nil)
	defer stub.Close()

	resp, body := doGet(t, app, "/api/analytics/content-decay")
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200 (graceful)", resp.StatusCode)
	}
	if body["available"] != false || body["reason"] != "addon_required" {
		t.Fatalf("expected addon_required envelope, got %v", body)
	}
}

func TestInsightNotConfiguredWhenNoProfile(t *testing.T) {
	app, stub := insightApp(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be called when profile is empty")
	}, func(context.Context) (string, error) { return "", nil }, nil)
	defer stub.Close()

	resp, body := doGet(t, app, "/api/analytics/posting-frequency")
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if body["available"] != false || body["reason"] != "not_configured" {
		t.Fatalf("expected not_configured envelope, got %v", body)
	}
}

func TestInsightRejectsBadSource(t *testing.T) {
	app, stub := insightApp(func(w http.ResponseWriter, r *http.Request) {}, okProfile, nil)
	defer stub.Close()
	resp, _ := doGet(t, app, "/api/analytics/best-times?source=bogus")
	if resp.StatusCode != 400 {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
}

func TestFollowersServedFromSnapshots(t *testing.T) {
	fr := &insightFakeFollowerRepo{
		summary: []repository.FollowerAccountSummary{
			{SocialAccountID: "acc-1", Platform: "instagram", Username: "acme", CurrentFollowers: 10432, Growth: 128, GrowthPercentage: 1.24, DataPoints: 30},
		},
		series: []repository.FollowerSeriesPoint{
			{SocialAccountID: "acc-1", PointDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), Followers: 10304},
			{SocialAccountID: "acc-1", PointDate: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC), Followers: 10432},
		},
	}
	app, stub := insightApp(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("followers read must not call the publisher")
	}, okProfile, fr)
	defer stub.Close()

	resp, body := doGet(t, app, "/api/analytics/followers?granularity=daily")
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if body["available"] != true {
		t.Fatalf("available: got %v", body)
	}
	data := body["data"].(map[string]any)
	accounts := data["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts: got %v", data["accounts"])
	}
	series := data["series"].(map[string]any)
	pts := series["acc-1"].([]any)
	if len(pts) != 2 {
		t.Fatalf("series acc-1: got %v", series["acc-1"])
	}
}

func TestFollowersRejectsBadGranularity(t *testing.T) {
	app, stub := insightApp(func(w http.ResponseWriter, r *http.Request) {}, okProfile, &insightFakeFollowerRepo{})
	defer stub.Close()
	resp, _ := doGet(t, app, "/api/analytics/followers?granularity=hourly")
	if resp.StatusCode != 400 {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
}

func TestFollowersUnavailableWhenNoRepo(t *testing.T) {
	app, stub := insightApp(func(w http.ResponseWriter, r *http.Request) {}, okProfile, nil)
	defer stub.Close()
	resp, _ := doGet(t, app, "/api/analytics/followers")
	if resp.StatusCode != 503 {
		t.Fatalf("status: got %d want 503", resp.StatusCode)
	}
}
