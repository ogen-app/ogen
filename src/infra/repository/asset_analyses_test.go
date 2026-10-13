package repository_test

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type analysisFixture struct {
	db        *bun.DB
	ctx       context.Context
	analyses  repository.AssetAnalysisRepository
	ideas     repository.IdeaRepository
	assets    repository.AssetRepository
	campaigns repository.CampaignRepository
}

func newAnalysisFixture(t *testing.T) *analysisFixture {
	t.Helper()
	db := openMigratedDB(t)
	return &analysisFixture{
		db:        db,
		ctx:       tenantCtx(),
		analyses:  repository.NewAssetAnalysisRepository(db),
		ideas:     repository.NewIdeaRepository(db),
		assets:    repository.NewAssetRepository(db, repository.NewTagRepository(db), repository.NewAssetFileRepository(db)),
		campaigns: repository.NewCampaignRepository(db, nil, nil, nil),
	}
}

func (f *analysisFixture) seedAsset(t *testing.T, id string) {
	t.Helper()
	now := time.Now().UTC()
	a := &models.Asset{
		ID: id, Title: "Title " + id, Status: models.AssetStatusReady, Type: new(models.AssetTypeDocument),
		TagIDs: models.StringSlice{}, CreatedBy: "user-1", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := f.db.NewInsert().Model(a).Exec(f.ctx); err != nil {
		t.Fatalf("seed asset %s: %v", id, err)
	}
}

// save writes a ready result for the lens with ideas titled titles.
func (f *analysisFixture) save(t *testing.T, a *models.AssetAnalysis, score int, titles ...string) {
	t.Helper()
	a.Status = models.AssetAnalysisStatusReady
	a.Summary = "summary"
	a.Result = models.AssetAnalysisResult{KeyPoints: []string{"k"}}
	if a.CampaignID != nil {
		a.Result.Relevance = &models.AnalysisRelevance{Score: score, Why: "why"}
	}
	ideas := make([]models.Idea, 0, len(titles))
	for _, title := range titles {
		id, err := models.NewID()
		if err != nil {
			t.Fatal(err)
		}
		ideas = append(ideas, models.Idea{
			ID: id, Title: title, CampaignID: a.CampaignID, Origin: models.IdeaOriginAssetAnalysis,
			AssetAnalysisID: new(a.ID), CreatedByName: "Ogen",
		})
	}
	err := f.db.RunInTx(f.ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := f.analyses.LockLens(ctx, tx, a.AssetID, a.CampaignID); err != nil {
			return err
		}
		return f.analyses.SaveResult(ctx, tx, a, ideas)
	})
	if err != nil {
		t.Fatalf("save result: %v", err)
	}
}

func (f *analysisFixture) ideaTitles(t *testing.T, filter repository.IdeaListFilter) []string {
	t.Helper()
	ideas, err := f.ideas.List(f.ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(ideas))
	for _, idea := range ideas {
		out = append(out, idea.Title)
	}
	return out
}

func (f *analysisFixture) triage(t *testing.T, title string) {
	t.Helper()
	ideas, err := f.ideas.List(f.ctx, repository.IdeaListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range ideas {
		if ideas[i].Title != title {
			continue
		}
		now := time.Now().UTC()
		ideas[i].Verdict = new(models.IdeaVerdictYes)
		ideas[i].DecidedAt = &now
		if _, err := f.ideas.Update(f.ctx, &ideas[i], "verdict", "decided_at"); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("no idea %q", title)
}

func TestAssetAnalysisEnsureIsOnePerLens(t *testing.T) {
	f := newAnalysisFixture(t)
	f.seedAsset(t, "asset-1")
	campaignID := seedCampaign(t, f.campaigns, f.ctx, "Launch")

	first, err := f.analyses.Ensure(f.ctx, f.db, "asset-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.analyses.Ensure(f.ctx, f.db, "asset-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatalf("workspace lens duplicated: %s vs %s", first.ID, again.ID)
	}
	lens, err := f.analyses.Ensure(f.ctx, f.db, "asset-1", &campaignID)
	if err != nil {
		t.Fatal(err)
	}
	if lens.ID == first.ID || lens.Status != models.AssetAnalysisStatusPending {
		t.Fatalf("campaign lens = %+v", lens)
	}

	all, err := f.analyses.ListByAsset(f.ctx, "asset-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].CampaignID != nil || all[1].CampaignName == nil || *all[1].CampaignName != "Launch" {
		t.Fatalf("ListByAsset = %+v", all)
	}
}

func TestAssetAnalysisRerunKeepsTriagedIdeas(t *testing.T) {
	f := newAnalysisFixture(t)
	f.seedAsset(t, "asset-1")
	a, err := f.analyses.Ensure(f.ctx, f.db, "asset-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.save(t, a, 0, "keep me", "drop me")
	f.triage(t, "keep me")

	f.save(t, a, 0, "fresh")

	got := f.ideaTitles(t, repository.IdeaListFilter{AssetID: "asset-1"})
	if len(got) != 2 || got[0] != "keep me" || got[1] != "fresh" {
		t.Fatalf("ideas after re-run = %v, want [keep me fresh]", got)
	}
	origin := models.IdeaOriginAssetAnalysis
	ideas, err := f.ideas.List(f.ctx, repository.IdeaListFilter{Origin: &origin})
	if err != nil {
		t.Fatal(err)
	}
	for _, idea := range ideas {
		if idea.SourceAsset == nil || idea.SourceAsset.ID != "asset-1" || idea.SourceAsset.Title != "Title asset-1" {
			t.Fatalf("source asset = %+v", idea.SourceAsset)
		}
	}
	manual := models.IdeaOriginManual
	if got := f.ideaTitles(t, repository.IdeaListFilter{Origin: &manual}); len(got) != 0 {
		t.Fatalf("manual ideas = %v, want none", got)
	}
}

func TestAssetAnalysisDeleteLensAndAsset(t *testing.T) {
	f := newAnalysisFixture(t)
	f.seedAsset(t, "asset-1")
	campaignID := seedCampaign(t, f.campaigns, f.ctx, "Launch")
	ws, err := f.analyses.Ensure(f.ctx, f.db, "asset-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	lens, err := f.analyses.Ensure(f.ctx, f.db, "asset-1", &campaignID)
	if err != nil {
		t.Fatal(err)
	}
	f.save(t, ws, 0, "ws kept", "ws inbox")
	f.save(t, lens, 80, "lens kept", "lens inbox")
	f.triage(t, "ws kept")
	f.triage(t, "lens kept")

	// openMigratedDB seeds with FK triggers off; the cascades under test need
	// them back on. The pool holds a single connection, so this sticks.
	if _, err := f.db.Exec("SET session_replication_role = origin"); err != nil {
		t.Fatal(err)
	}
	if err := f.analyses.DeleteLens(f.ctx, f.db, "asset-1", campaignID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.analyses.GetLens(f.ctx, "asset-1", &campaignID); err == nil {
		t.Fatal("lens survived DeleteLens")
	}
	got := f.ideaTitles(t, repository.IdeaListFilter{})
	if len(got) != 3 {
		t.Fatalf("after detach ideas = %v, want ws kept, ws inbox, lens kept", got)
	}

	deleted, err := f.assets.Delete(f.ctx, "asset-1")
	if err != nil || !deleted {
		t.Fatalf("asset delete = %v, %v", deleted, err)
	}
	ideas, err := f.ideas.List(f.ctx, repository.IdeaListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ideas) != 2 {
		t.Fatalf("after asset delete ideas = %d, want the 2 triaged", len(ideas))
	}
	for _, idea := range ideas {
		if idea.AssetAnalysisID != nil || idea.Origin != models.IdeaOriginAssetAnalysis {
			t.Fatalf("triaged idea %q after delete: link=%v origin=%q", idea.Title, idea.AssetAnalysisID, idea.Origin)
		}
	}
}

// TestAssetAnalysisWorkspaceDigests: the gist comes back as stored, and the
// summary head is read (capped) only for lenses without one.
func TestAssetAnalysisWorkspaceDigests(t *testing.T) {
	f := newAnalysisFixture(t)
	longSummary := strings.Repeat("Older analysis prose. ", 60) // > 800 runes
	for assetID, gist := range map[string]string{"with-gist": "Reference architectures for RAG.", "no-gist": ""} {
		f.seedAsset(t, assetID)
		a, err := f.analyses.Ensure(f.ctx, f.db, assetID, nil)
		if err != nil {
			t.Fatal(err)
		}
		f.save(t, a, 0)
		if _, err := f.db.NewUpdate().Model((*models.AssetAnalysis)(nil)).
			Set("gist = ?", gist).Set("summary = ?", longSummary).
			Where("id = ?", a.ID).Exec(f.ctx); err != nil {
			t.Fatal(err)
		}
	}

	got, err := f.analyses.WorkspaceDigests(f.ctx, []string{"with-gist", "no-gist"})
	if err != nil {
		t.Fatal(err)
	}
	if d := got["with-gist"]; d.Status != models.AssetAnalysisStatusReady || d.Gist != "Reference architectures for RAG." || d.SummaryHead != "" {
		t.Fatalf("with-gist digest = %+v", d)
	}
	d := got["no-gist"]
	if d.Gist != "" || utf8.RuneCountInString(d.SummaryHead) != 800 || !strings.HasPrefix(d.SummaryHead, "Older analysis prose.") {
		t.Fatalf("no-gist digest = gist %q, head %d runes", d.Gist, utf8.RuneCountInString(d.SummaryHead))
	}
}

func TestAssetAnalysisCampaignReads(t *testing.T) {
	f := newAnalysisFixture(t)
	f.seedAsset(t, "low")
	f.seedAsset(t, "high")
	campaignID := seedCampaign(t, f.campaigns, f.ctx, "Launch")
	for _, assetID := range []string{"low", "high"} {
		if _, err := f.analyses.Ensure(f.ctx, f.db, assetID, nil); err != nil {
			t.Fatal(err)
		}
	}
	low, err := f.analyses.Ensure(f.ctx, f.db, "low", &campaignID)
	if err != nil {
		t.Fatal(err)
	}
	high, err := f.analyses.Ensure(f.ctx, f.db, "high", &campaignID)
	if err != nil {
		t.Fatal(err)
	}
	f.save(t, low, 10)
	f.save(t, high, 90)

	got, err := f.analyses.ListByCampaign(f.ctx, campaignID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].AssetID != "high" || got[1].AssetID != "low" {
		t.Fatalf("ListByCampaign order = %+v", got)
	}

	digests, err := f.analyses.WorkspaceDigests(f.ctx, []string{"low", "high", "none"})
	if err != nil {
		t.Fatal(err)
	}
	if len(digests) != 2 || digests["low"].Status != models.AssetAnalysisStatusPending {
		t.Fatalf("WorkspaceDigests = %v", digests)
	}

	if _, err := f.campaigns.Delete(f.ctx, campaignID); err != nil {
		t.Fatal(err)
	}
	if got, err := f.analyses.ListByCampaign(f.ctx, campaignID); err != nil || len(got) != 0 {
		t.Fatalf("lenses of a deleted campaign = %v, %v", got, err)
	}
	if _, err := f.analyses.GetLens(f.ctx, "high", &campaignID); err == nil {
		t.Fatal("GetLens returned a deleted campaign's lens")
	}
}

func TestAssetAnalysisRunLedger(t *testing.T) {
	f := newAnalysisFixture(t)
	since := time.Now().UTC().AddDate(0, 0, -1)
	for range 3 {
		if _, err := f.analyses.RecordRun(f.ctx, "analysis-1"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.NewUpdate().
		Model((*models.AssetAnalysisRun)(nil)).
		Set("created_at = ?", since.AddDate(0, -1, 0)).
		Where("id = (SELECT min(id) FROM asset_analysis_runs)").
		Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	n, err := f.analyses.CountRunsSince(f.ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("runs since = %d, want 2", n)
	}
}
