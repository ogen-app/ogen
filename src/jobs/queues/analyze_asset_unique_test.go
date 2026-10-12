package queues_test

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
)

// A burst of saves queues one waiting job per lens, at the first save's
// debounce; another lens gets its own.
func TestEditedAnalysesCollapseWhileWaiting(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	if err := jobs.MigrateRiver(t.Context(), db.DB); err != nil {
		t.Fatal(err)
	}
	enq, err := queues.NewInsertOnlyEnqueuer(db)
	if err != nil {
		t.Fatal(err)
	}

	first := time.Now().Add(5 * time.Minute)
	for i := range 5 {
		at := first.Add(time.Duration(i) * time.Second)
		if err := enq.EnqueueAnalyzeAsset(t.Context(), "t1", "note-1", nil, assetanalysis.ReasonEdited, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := enq.EnqueueAnalyzeAsset(t.Context(), "t1", "note-1", new("camp-1"), assetanalysis.ReasonEdited, first); err != nil {
		t.Fatal(err)
	}

	var rows []struct {
		State       string    `bun:"state"`
		ScheduledAt time.Time `bun:"scheduled_at"`
	}
	if err := db.NewSelect().Table("river_job").Column("state", "scheduled_at").
		Where("kind = ?", queues.AnalyzeAssetKind).OrderExpr("id").Scan(t.Context(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("jobs = %d, want one per lens", len(rows))
	}
	for _, r := range rows {
		if r.State != "scheduled" || r.ScheduledAt.Sub(first).Abs() > time.Second {
			t.Fatalf("job = %s at %v, want scheduled at the first save's debounce %v", r.State, r.ScheduledAt, first)
		}
	}

	// River rejects unique opts it cannot enforce, so every analysis kind
	// must insert for real.
	for range 2 {
		if err := enq.EnqueueReanalyzeCampaign(t.Context(), "t1", "camp-1", first); err != nil {
			t.Fatal(err)
		}
		if _, err := enq.Client.Insert(t.Context(), queues.BackfillAssetAnalysesTask{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{queues.ReanalyzeCampaignKind, queues.BackfillAssetAnalysesKind} {
		n, err := db.NewSelect().Table("river_job").Where("kind = ?", kind).Count(t.Context())
		if err != nil || n != 1 {
			t.Fatalf("%s jobs = %d, %v; want 1", kind, n, err)
		}
	}
}
