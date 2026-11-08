package repository_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Open ideas of a scope are newest first, without turned-down ideas or ideas
// a post was made from; the workspace scope is the ideas in no campaign.
func TestIdeas_OpenIdeas(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewIdeaRepository(db)
	links := repository.NewPostIdeaRepository(db)
	ctx := tenantctx.With(t.Context(), "tenant-idea-open")

	c := &models.Campaign{ID: "open-live", Name: "Live", CampaignTypeID: "t", CreatedBy: "u1",
		AssetIDs: models.StringSlice{}, PublishingDays: models.StringSlice{}, TargetPlatforms: models.CampaignPlatforms{}}
	if _, err := db.NewInsert().Model(c).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	no, live := models.IdeaVerdictNo, "open-live"
	for n, i := range []*models.Idea{
		{ID: "o-ws-old", Title: "old"},
		{ID: "o-ws-new", Title: "new"},
		{ID: "o-ws-no", Title: "turned down", Verdict: &no, DecidedAt: &base, DecidedBy: new("u1")},
		{ID: "o-ws-posted", Title: "posted"},
		{ID: "o-camp", Title: "campaign", CampaignID: &live},
	} {
		i.CreatedAt = base.Add(time.Duration(n) * time.Hour)
		if err := repo.Create(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	if err := links.Link(ctx, db, "post-open", []string{"o-ws-posted"}); err != nil {
		t.Fatal(err)
	}
	ids := func(list []models.Idea) []string {
		var out []string
		for _, i := range list {
			out = append(out, i.ID)
		}
		return out
	}

	got, more, err := repo.OpenIdeas(ctx, "", 10)
	if err != nil || more || !slices.Equal(ids(got), []string{"o-ws-new", "o-ws-old"}) {
		t.Fatalf("workspace = %v, more %v, %v", ids(got), more, err)
	}
	if got, more, _ := repo.OpenIdeas(ctx, "", 1); !more || !slices.Equal(ids(got), []string{"o-ws-new"}) {
		t.Fatalf("capped = %v, more %v", ids(got), more)
	}
	if got, _, _ := repo.OpenIdeas(ctx, live, 10); !slices.Equal(ids(got), []string{"o-camp"}) {
		t.Fatalf("campaign = %v", ids(got))
	}
	other := tenantctx.With(context.Background(), "tenant-other-open")
	if got, _, _ := repo.OpenIdeas(other, "", 10); len(got) != 0 {
		t.Fatalf("another tenant read %v", ids(got))
	}
}

// A merge reads its set under lock, writes the kept idea and deletes the
// rest in one transaction; a refused plan changes nothing.
func TestIdeas_MergeIdeas(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewIdeaRepository(db)
	links := repository.NewPostIdeaRepository(db)
	ctx := tenantctx.With(t.Context(), "tenant-idea-merge")
	for _, i := range []*models.Idea{
		{ID: "m-keep", Title: "keep", AssetIDs: []string{"a1"}},
		{ID: "m-dup", Title: "dup", AssetIDs: []string{"a2"}},
		{ID: "m-posted", Title: "posted"},
	} {
		if err := repo.Create(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	if err := links.Link(ctx, db, "post-merge", []string{"m-posted"}); err != nil {
		t.Fatal(err)
	}
	other := tenantctx.With(context.Background(), "tenant-other-merge")
	if set, err := repo.MergeSet(other, []string{"m-keep"}); err != nil || len(set.Ideas) != 0 {
		t.Fatalf("another tenant's merge set = %+v, %v", set, err)
	}

	set, err := repo.MergeSet(ctx, []string{"m-keep", "m-dup", "m-posted", "m-gone"})
	if err != nil || len(set.Ideas) != 3 || !slices.Equal(set.Compiled, []string{"m-posted"}) {
		t.Fatalf("set = %+v, %v", set, err)
	}

	refused := errors.New("refused")
	_, err = repo.MergeIdeas(ctx, []string{"m-keep", "m-dup"}, func(repository.IdeaMergeSet) (repository.IdeaMerge, error) {
		return repository.IdeaMerge{}, refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("refused plan = %v", err)
	}

	// The kept idea is written, then the delete fails on a row that still
	// references the duplicate: nothing changes. openMigratedDB seeds with FK
	// triggers off; the single pooled connection keeps them back on.
	for _, q := range []string{
		"SET session_replication_role = origin",
		"CREATE TABLE merge_blocker (idea_id TEXT REFERENCES ideas (id))",
		"INSERT INTO merge_blocker VALUES ('m-dup')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_, err = repo.MergeIdeas(ctx, []string{"m-keep", "m-dup"}, func(set repository.IdeaMergeSet) (repository.IdeaMerge, error) {
		keep := set.Ideas[0]
		keep.Note = "half merged"
		return repository.IdeaMerge{Keep: &keep, Columns: []string{"note"}, Delete: []string{"m-dup"}}, nil
	})
	if err == nil {
		t.Fatal("a failed delete reported a merge")
	}
	if _, err := db.Exec("DROP TABLE merge_blocker"); err != nil {
		t.Fatal(err)
	}
	if keep, err := repo.GetByID(ctx, "m-keep"); err != nil || keep.Note != "" {
		t.Fatalf("keep after a failed merge = %+v, %v", keep, err)
	}
	if _, err := repo.GetByID(ctx, "m-dup"); err != nil {
		t.Fatalf("dup after a failed merge: %v", err)
	}

	kept, err := repo.MergeIdeas(ctx, []string{"m-keep", "m-dup"}, func(set repository.IdeaMergeSet) (repository.IdeaMerge, error) {
		if len(set.Ideas) != 2 || len(set.Compiled) != 0 {
			t.Errorf("locked set = %+v", set)
		}
		keep := set.Ideas[0]
		keep.AssetIDs = []string{"a1", "a2"}
		keep.Note = "merged"
		return repository.IdeaMerge{Keep: &keep, Columns: []string{"asset_ids", "note"}, Delete: []string{"m-dup"}}, nil
	})
	if err != nil || kept.ID != "m-keep" || kept.Note != "merged" {
		t.Fatalf("kept = %+v, %v; want it read back", kept, err)
	}
	keep, err := repo.GetByID(ctx, "m-keep")
	if err != nil || keep.Note != "merged" || !slices.Equal(keep.AssetIDs, []string{"a1", "a2"}) {
		t.Fatalf("keep = %+v, %v", keep, err)
	}
	if _, err := repo.GetByID(ctx, "m-dup"); err == nil {
		t.Fatal("the merged idea is still there")
	}
}

// An untriaged analysis idea other ideas were merged into outlives the
// clean-ups of its asset's inbox: a re-run, and the asset's delete.
func TestIdeas_MergedAnalysisIdeaSurvivesCleanup(t *testing.T) {
	f := newAnalysisFixture(t)
	f.seedAsset(t, "asset-m")
	a, err := f.analyses.Ensure(f.ctx, f.db, "asset-m", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.save(t, a, 0, "kept", "merged away", "inbox")
	byTitle := map[string]string{}
	ideas, err := f.ideas.List(f.ctx, repository.IdeaListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ideas {
		byTitle[i.Title] = i.ID
	}
	_, err = f.ideas.MergeIdeas(f.ctx, []string{byTitle["kept"], byTitle["merged away"]}, func(set repository.IdeaMergeSet) (repository.IdeaMerge, error) {
		keep := set.Ideas[slices.IndexFunc(set.Ideas, func(i models.Idea) bool { return i.Title == "kept" })]
		keep.MergedAt = new(time.Now().UTC())
		return repository.IdeaMerge{Keep: &keep, Columns: []string{"merged_at"}, Delete: []string{byTitle["merged away"]}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	f.save(t, a, 0, "fresh")
	if got := f.ideaTitles(t, repository.IdeaListFilter{}); !slices.Equal(got, []string{"kept", "fresh"}) {
		t.Fatalf("after a re-run = %v, want the merged idea kept", got)
	}
	if _, err := f.db.Exec("SET session_replication_role = origin"); err != nil {
		t.Fatal(err)
	}
	if deleted, err := f.assets.Delete(f.ctx, "asset-m"); err != nil || !deleted {
		t.Fatalf("asset delete = %v, %v", deleted, err)
	}
	if got := f.ideaTitles(t, repository.IdeaListFilter{}); !slices.Equal(got, []string{"kept"}) {
		t.Fatalf("after the asset's delete = %v, want the merged idea kept", got)
	}
}
