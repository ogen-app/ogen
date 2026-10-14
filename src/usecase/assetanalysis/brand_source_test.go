package assetanalysis_test

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
)

// markBrandSource stamps a seeded asset as brought in to read a brand from.
func (f *fixture) markBrandSource(t *testing.T, id string) {
	t.Helper()
	if _, err := f.db.NewUpdate().Model((*models.Asset)(nil)).
		Set("origin = ?", models.AssetOriginBrandDistill).
		Where("id = ?", id).
		Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
}

func TestBrandSourceSkipsItsWorkspaceLens(t *testing.T) {
	f := newFixture(t, nil)
	f.seedAsset(t, "deck", models.AssetTypePDF, models.AssetStatusReady, "")
	f.markBrandSource(t, "deck")
	f.seedCampaign(t, "camp-a", "deck")

	f.svc.ScheduleAsset(f.ctx, tenant, "deck")

	want := []queued{{"deck", "camp-a", assetanalysis.ReasonIngested}}
	if len(f.jobs.analyses) != 1 || f.jobs.analyses[0] != want[0] {
		t.Fatalf("queued = %+v, want only the campaign lens", f.jobs.analyses)
	}
	lens := f.lens(t, "deck", nil)
	if lens.Status != models.AssetAnalysisStatusSkipped || lens.FailureCode != models.AnalysisCodeBrandSource {
		t.Fatalf("workspace lens = %s/%s, want skipped/brand_source", lens.Status, lens.FailureCode)
	}

	t.Run("a second ingest leaves the skipped lens alone", func(t *testing.T) {
		f.jobs.analyses = nil
		f.svc.ScheduleAsset(f.ctx, tenant, "deck")
		if got := f.lens(t, "deck", nil); got.Status != models.AssetAnalysisStatusSkipped {
			t.Fatalf("lens = %s", got.Status)
		}
	})

	t.Run("analysing it by hand queues the lens", func(t *testing.T) {
		f.jobs.analyses = nil
		if err := f.svc.Retry(f.ctx, tenant, "deck", nil); err != nil {
			t.Fatal(err)
		}
		if len(f.jobs.analyses) != 1 || f.jobs.analyses[0] != (queued{"deck", "", assetanalysis.ReasonRetry}) {
			t.Fatalf("queued = %+v", f.jobs.analyses)
		}
	})
}

func TestBrandSourceNoteEditSkipsItsWorkspaceLens(t *testing.T) {
	f := newFixture(t, nil)
	f.seedAsset(t, "note", models.AssetTypeMarkdown, models.AssetStatusReady, "text")
	f.markBrandSource(t, "note")

	f.svc.ScheduleEdited(f.ctx, tenant, "note")

	if len(f.jobs.analyses) != 0 {
		t.Fatalf("queued = %+v, want nothing", f.jobs.analyses)
	}
	if lens := f.lens(t, "note", nil); lens.Status != models.AssetAnalysisStatusSkipped {
		t.Fatalf("lens = %s, want skipped", lens.Status)
	}
}

func TestBackfillPassesOverBrandSources(t *testing.T) {
	f := newFixture(t, nil)
	f.seedAsset(t, "deck", models.AssetTypePDF, models.AssetStatusReady, "")
	f.markBrandSource(t, "deck")
	f.seedAsset(t, "md-1", models.AssetTypeMarkdown, models.AssetStatusReady, "text")
	f.seedCampaign(t, "camp-a", "deck")

	if _, err := f.svc.Backfill(f.ctx, 10); err != nil {
		t.Fatal(err)
	}
	want := map[queued]bool{
		{"md-1", "", assetanalysis.ReasonBackfill}:       true,
		{"deck", "camp-a", assetanalysis.ReasonBackfill}: true,
	}
	if len(f.jobs.analyses) != len(want) {
		t.Fatalf("queued = %+v, want %v", f.jobs.analyses, want)
	}
	for _, q := range f.jobs.analyses {
		if !want[q] {
			t.Fatalf("queued %+v; a brand source's workspace lens must not be backfilled", q)
		}
	}
}
