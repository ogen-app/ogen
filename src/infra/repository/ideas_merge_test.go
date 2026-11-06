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
	err = repo.MergeIdeas(ctx, []string{"m-keep", "m-dup"}, func(repository.IdeaMergeSet) (repository.IdeaMerge, error) {
		return repository.IdeaMerge{}, refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("refused plan = %v", err)
	}

	err = repo.MergeIdeas(ctx, []string{"m-keep", "m-dup"}, func(set repository.IdeaMergeSet) (repository.IdeaMerge, error) {
		if len(set.Ideas) != 2 || len(set.Compiled) != 0 {
			t.Errorf("locked set = %+v", set)
		}
		keep := set.Ideas[0]
		keep.AssetIDs = []string{"a1", "a2"}
		keep.Note = "merged"
		return repository.IdeaMerge{Keep: &keep, Columns: []string{"asset_ids", "note"}, Delete: []string{"m-dup"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	keep, err := repo.GetByID(ctx, "m-keep")
	if err != nil || keep.Note != "merged" || !slices.Equal(keep.AssetIDs, []string{"a1", "a2"}) {
		t.Fatalf("keep = %+v, %v", keep, err)
	}
	if _, err := repo.GetByID(ctx, "m-dup"); err == nil {
		t.Fatal("the merged idea is still there")
	}
}
