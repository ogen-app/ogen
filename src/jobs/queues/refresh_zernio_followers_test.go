package queues_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// fakeFollowerRepo records upserts and can pretend a latest point exists.
type fakeFollowerRepo struct {
	mu     sync.Mutex
	rows   []models.FollowerSnapshot
	latest time.Time
}

func (r *fakeFollowerRepo) InsertMany(_ context.Context, rows []models.FollowerSnapshot) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, rows...)
	return len(rows), nil
}
func (r *fakeFollowerRepo) Summary(context.Context, string) ([]repository.FollowerAccountSummary, error) {
	return nil, nil
}
func (r *fakeFollowerRepo) Series(context.Context, repository.FollowerSeriesOptions) ([]repository.FollowerSeriesPoint, error) {
	return nil, nil
}
func (r *fakeFollowerRepo) LatestPointDate(context.Context) (time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.latest, nil
}

func newFollowerProcessor(stub *stubZernio, acct *fakeAccountRepo, fr *fakeFollowerRepo, settings *fakeSettings) *queues.RefreshZernioFollowersProcessor {
	return &queues.RefreshZernioFollowersProcessor{
		Deps: queues.ZernioDeps{
			SocialAccountRepo: acct,
			FollowerRepo:      fr,
			Client:            zernio.NewClient(zernio.StaticKey("k"), stub.URL, zernio.ClientOpts{Timeout: 5 * time.Second}),
		},
		Settings: settings,
	}
}

func TestFollowerRefreshUpsertsPerTenant(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()

	stub.handle("GET", "/accounts/follower-stats", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("profileId") != "prof-1" {
			t.Errorf("profileId: got %q want prof-1", q.Get("profileId"))
		}
		if q.Get("granularity") != "daily" {
			t.Errorf("granularity: got %q want daily", q.Get("granularity"))
		}
		if q.Get("fromDate") == "" {
			t.Errorf("fromDate should be set (first-run backfill)")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"accounts": []any{
				map[string]any{"_id": "acc-1", "platform": "instagram", "username": "acme",
					"currentFollowers": 128, "growth": 28, "growthPercentage": 28.0, "dataPoints": 2},
			},
			"stats": map[string]any{
				"acc-1": []any{
					map[string]any{"date": "2026-07-01", "followers": 100},
					map[string]any{"date": "2026-07-02", "followers": 128},
				},
			},
			"granularity": "daily",
		})
	})

	acct := &fakeAccountRepo{accounts: map[string][]models.SocialAccount{
		"prof-1": {{ID: "acc-1", Platform: "instagram", ProfileID: "prof-1", TenantScoped: models.TenantScoped{TenantID: "t1"}}},
	}}
	fr := &fakeFollowerRepo{}
	settings := newFakeSettings()

	proc := newFollowerProcessor(stub, acct, fr, settings)
	if err := proc.Process(t.Context(), queues.RefreshZernioFollowersTask{}); err != nil {
		t.Fatalf("process: %v", err)
	}

	if len(fr.rows) != 2 {
		t.Fatalf("rows: got %d want 2", len(fr.rows))
	}
	// Rows carry series follower counts + summary-derived platform/username/growth.
	// Note: tenant_id is stamped by the TenantScoped hook at the real bun
	// insert (covered by the repo test); the in-memory fake bypasses it, so we
	// assert the builder's own fields here, not row.TenantID.
	byDate := map[string]models.FollowerSnapshot{}
	for _, row := range fr.rows {
		if row.SocialAccountID != "acc-1" {
			t.Errorf("row account wrong: %+v", row)
		}
		if row.Platform != "instagram" || row.Username != "acme" || row.Growth != 28 {
			t.Errorf("row denormalisation wrong: %+v", row)
		}
		byDate[row.PointDate.Format("2006-01-02")] = row
	}
	if byDate["2026-07-01"].Followers != 100 || byDate["2026-07-02"].Followers != 128 {
		t.Errorf("series counts not mapped: %+v", byDate)
	}

	// Health recorded under the tenant's context.
	if v, _, _ := settings.Get(tctx("t1"), zernio.SettingFollowersLastRefreshStatus); v != zernio.SyncStatusOK {
		t.Errorf("follower refresh status: got %q want ok", v)
	}
	if v, _, _ := settings.Get(tctx("t1"), zernio.SettingFollowersLastRefreshAt); v == "" {
		t.Errorf("follower last_refresh_at not written")
	}
}

// TestFollowerRefreshIncrementalFromDate: when snapshots already exist, the
// fetch requests only points newer than the latest recorded point_date.
func TestFollowerRefreshIncrementalFromDate(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()

	var gotFrom string
	var mu sync.Mutex
	stub.handle("GET", "/accounts/follower-stats", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotFrom = r.URL.Query().Get("fromDate")
		mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"accounts": []any{}, "stats": map[string]any{}})
	})

	acct := &fakeAccountRepo{accounts: map[string][]models.SocialAccount{
		"prof-1": {{ID: "acc-1", Platform: "instagram", ProfileID: "prof-1", TenantScoped: models.TenantScoped{TenantID: "t1"}}},
	}}
	fr := &fakeFollowerRepo{latest: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)}
	settings := newFakeSettings()

	proc := newFollowerProcessor(stub, acct, fr, settings)
	if err := proc.Process(t.Context(), queues.RefreshZernioFollowersTask{}); err != nil {
		t.Fatalf("process: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotFrom != "2026-07-20" {
		t.Errorf("fromDate: got %q want 2026-07-20 (incremental from latest point)", gotFrom)
	}
}

// TestFollowerRefreshNoAccountsNoCall: a tenant with no active accounts is not
// enumerated, so no upstream call happens.
func TestFollowerRefreshNoAccountsNoCall(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	var called bool
	stub.handle("GET", "/accounts/follower-stats", func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeJSON(w, http.StatusOK, map[string]any{"accounts": []any{}, "stats": map[string]any{}})
	})

	acct := &fakeAccountRepo{accounts: map[string][]models.SocialAccount{}}
	fr := &fakeFollowerRepo{}
	proc := newFollowerProcessor(stub, acct, fr, newFakeSettings())
	if err := proc.Process(tenantctx.WithSystem(t.Context()), queues.RefreshZernioFollowersTask{}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if called {
		t.Errorf("no accounts → follower-stats endpoint must not be called")
	}
	if len(fr.rows) != 0 {
		t.Errorf("rows: got %d want 0", len(fr.rows))
	}
}

// TestFollowerRefreshSkipsUnmatchedAccounts: a series entry with no matching
// summary account is skipped entirely (no row with empty platform/misleading
// zero growth), while matched accounts are still recorded.
func TestFollowerRefreshSkipsUnmatchedAccounts(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()

	stub.handle("GET", "/accounts/follower-stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"accounts": []any{
				map[string]any{"_id": "acc-1", "platform": "instagram", "username": "acme",
					"currentFollowers": 128, "growth": 28, "growthPercentage": 28.0, "dataPoints": 1},
			},
			"stats": map[string]any{
				"acc-1":     []any{map[string]any{"date": "2026-07-01", "followers": 100}},
				"acc-ghost": []any{map[string]any{"date": "2026-07-01", "followers": 999}},
			},
			"granularity": "daily",
		})
	})

	acct := &fakeAccountRepo{accounts: map[string][]models.SocialAccount{
		"prof-1": {{ID: "acc-1", Platform: "instagram", ProfileID: "prof-1", TenantScoped: models.TenantScoped{TenantID: "t1"}}},
	}}
	fr := &fakeFollowerRepo{}
	proc := newFollowerProcessor(stub, acct, fr, newFakeSettings())
	if err := proc.Process(t.Context(), queues.RefreshZernioFollowersTask{}); err != nil {
		t.Fatalf("process: %v", err)
	}

	if len(fr.rows) != 1 {
		t.Fatalf("rows: got %d want 1 (acc-ghost must be skipped)", len(fr.rows))
	}
	if fr.rows[0].SocialAccountID != "acc-1" {
		t.Errorf("unexpected account persisted: %+v", fr.rows[0])
	}
}
