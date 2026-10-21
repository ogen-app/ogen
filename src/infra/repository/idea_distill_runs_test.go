package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

func (f *analysisFixture) distillIdeas(t *testing.T, distills repository.IdeaDistillRepository, run *models.IdeaDistillRun, titles ...string) {
	t.Helper()
	ideas := make([]models.Idea, 0, len(titles))
	for _, title := range titles {
		id, err := models.NewID()
		if err != nil {
			t.Fatal(err)
		}
		ideas = append(ideas, models.Idea{
			ID: id, Title: title, CampaignID: run.CampaignID, Origin: models.IdeaOriginAssetAnalysis, CreatedByName: "Ogen",
		})
	}
	err := f.db.RunInTx(f.ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return distills.SaveIdeas(ctx, tx, run, ideas)
	})
	if err != nil {
		t.Fatalf("save distilled ideas: %v", err)
	}
}

func TestIdeaDistillOneActiveRunPerLens(t *testing.T) {
	f := newAnalysisFixture(t)
	distills := repository.NewIdeaDistillRepository(f.db)
	f.seedAsset(t, "asset-1")
	lens, err := f.analyses.Ensure(f.ctx, f.db, "asset-1", nil)
	if err != nil {
		t.Fatal(err)
	}

	first := &models.IdeaDistillRun{AnalysisID: lens.ID, AssetID: "asset-1", Focus: "pricing", Count: 5}
	if err := distills.Create(f.ctx, first); err != nil {
		t.Fatal(err)
	}
	second := &models.IdeaDistillRun{AnalysisID: lens.ID, AssetID: "asset-1", Count: 5}
	if err := distills.Create(f.ctx, second); !errors.Is(err, repository.ErrDistillActive) {
		t.Fatalf("second active run: err = %v, want ErrDistillActive", err)
	}
	active, err := distills.Active(f.ctx, lens.ID)
	if err != nil || active.ID != first.ID || active.Focus != "pricing" {
		t.Fatalf("Active = %+v, %v", active, err)
	}

	if err := distills.SetStatus(f.ctx, first.ID, models.IdeaDistillStatusFailed, models.AnalysisCodeModelUnavailable, "down"); err != nil {
		t.Fatal(err)
	}
	if _, err := distills.Active(f.ctx, lens.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Active after failure: err = %v, want sql.ErrNoRows", err)
	}
	got, err := distills.Get(f.ctx, first.ID)
	if err != nil || got.FinishedAt == nil || got.FailureCode != models.AnalysisCodeModelUnavailable {
		t.Fatalf("failed run = %+v, %v", got, err)
	}
	if err := distills.Create(f.ctx, second); err != nil {
		t.Fatalf("run after the first finished: %v", err)
	}
	runs, err := distills.ListByAnalysis(f.ctx, lens.ID, 20)
	if err != nil || len(runs) != 2 || runs[0].ID != second.ID {
		t.Fatalf("ListByAnalysis = %+v, %v", runs, err)
	}
}

func TestIdeaDistillIdeasSurviveRerun(t *testing.T) {
	f := newAnalysisFixture(t)
	distills := repository.NewIdeaDistillRepository(f.db)
	f.seedAsset(t, "asset-1")
	lens, err := f.analyses.Ensure(f.ctx, f.db, "asset-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.save(t, lens, 0, "auto")
	run := &models.IdeaDistillRun{AnalysisID: lens.ID, AssetID: "asset-1", Count: 3}
	if err := distills.Create(f.ctx, run); err != nil {
		t.Fatal(err)
	}
	f.distillIdeas(t, distills, run, "more one", "more two")

	got, err := distills.Get(f.ctx, run.ID)
	if err != nil || got.Status != models.IdeaDistillStatusReady || got.IdeasCount != 2 || got.FinishedAt == nil {
		t.Fatalf("saved run = %+v, %v", got, err)
	}
	ideas, err := f.ideas.List(f.ctx, repository.IdeaListFilter{AssetID: "asset-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, idea := range ideas {
		if idea.Title == "auto" {
			continue
		}
		if idea.DistillRunID == nil || *idea.DistillRunID != run.ID || idea.AssetAnalysisID == nil || *idea.AssetAnalysisID != lens.ID {
			t.Fatalf("distilled idea = %+v", idea)
		}
	}

	f.save(t, lens, 0, "auto again")

	titles := f.ideaTitles(t, repository.IdeaListFilter{AssetID: "asset-1"})
	if len(titles) != 3 || titles[0] != "more one" || titles[1] != "more two" || titles[2] != "auto again" {
		t.Fatalf("ideas after re-run = %v, want [more one more two auto again]", titles)
	}
}

func TestIdeaDistillDetachDropsUntriagedDistilledIdeas(t *testing.T) {
	f := newAnalysisFixture(t)
	distills := repository.NewIdeaDistillRepository(f.db)
	f.seedAsset(t, "asset-1")
	campaignID := seedCampaign(t, f.campaigns, f.ctx, "Launch")
	lens, err := f.analyses.Ensure(f.ctx, f.db, "asset-1", &campaignID)
	if err != nil {
		t.Fatal(err)
	}
	f.save(t, lens, 80, "auto")
	run := &models.IdeaDistillRun{AnalysisID: lens.ID, AssetID: "asset-1", CampaignID: &campaignID, Count: 3}
	if err := distills.Create(f.ctx, run); err != nil {
		t.Fatal(err)
	}
	f.distillIdeas(t, distills, run, "kept", "dropped")
	f.triage(t, "kept")

	if _, err := f.db.Exec("SET session_replication_role = origin"); err != nil {
		t.Fatal(err)
	}
	if err := f.analyses.DeleteLens(f.ctx, f.db, "asset-1", campaignID); err != nil {
		t.Fatal(err)
	}

	ideas, err := f.ideas.List(f.ctx, repository.IdeaListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ideas) != 1 || ideas[0].Title != "kept" || ideas[0].DistillRunID != nil {
		t.Fatalf("ideas after detach = %+v, want only the triaged one, unlinked", ideas)
	}
	if _, err := distills.Get(f.ctx, run.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("run after detach: err = %v, want sql.ErrNoRows", err)
	}
}

func TestIdeaDistillChargeLedger(t *testing.T) {
	f := newAnalysisFixture(t)
	distills := repository.NewIdeaDistillRepository(f.db)
	since := time.Now().Add(-time.Minute)

	kept, err := distills.RecordCharge(f.ctx, "run-1")
	if err != nil || kept == "" {
		t.Fatalf("RecordCharge = %q, %v", kept, err)
	}
	released, err := distills.RecordCharge(f.ctx, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := distills.DeleteCharge(f.ctx, released); err != nil {
		t.Fatal(err)
	}
	n, err := distills.CountChargesSince(f.ctx, since)
	if err != nil || n != 1 {
		t.Fatalf("CountChargesSince = %d, %v; want 1", n, err)
	}
	other := tenantctx.With(context.Background(), "other-tenant")
	if n, _ := distills.CountChargesSince(other, since); n != 0 {
		t.Fatalf("other tenant sees %d charges", n)
	}
}
