package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Every view counts what the list's Total says for the same query, whatever
// mix of filters and texts the batch holds.
func TestAssetCountViews_AgreesWithQuery(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	since := fx.now.Add(-5 * 24 * time.Hour)
	queries := []models.AssetQuery{
		{},
		{Title: anyOf("GARTNER", "deck")},
		{Title: noneOf("100%")},
		{Kind: anyOf("pdf", "note"), Status: noneOf("failed")},
		{Status: anyOf("pending")},
		{Campaign: anyOf("c-explicit")},
		{Campaign: anyOf("none"), Tag: noneOf("none")},
		{Tag: anyOf("tg-finance", "none")},
		{Ideas: anyOf("undecided")},
		{Ideas: noneOf("unread")},
		{UpdatedAfter: &since},
		{CreatedBefore: new(fx.now.Add(-75 * 24 * time.Hour))},
		{Text: "overwinter"},
		{Text: "overwinter", Kind: anyOf("web")},
		{Text: "pricing"},
		{Text: "the and"},
		{Text: "pricing", Tag: anyOf("tg-gartner")},
		// Paging, sort and grouping do not change a count.
		{Group: models.AssetGroupTag, Sort: models.AssetSortTitleAsc, Limit: 1, Facets: models.AssetFacets},
	}
	for i := range queries {
		queries[i] = fx.query(queries[i])
	}
	got, err := fx.repo.CountViews(fx.ctx, queries)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(queries) {
		t.Fatalf("%d counts for %d views", len(got), len(queries))
	}
	for i, q := range queries {
		if want := fx.run(t, q).Total; got[i].Total != want {
			t.Errorf("view %d (%+v): total = %d, want %d", i, q, got[i].Total, want)
		}
	}
}

// A batch is not bound by the 100-argument limit of SQL functions.
func TestAssetCountViews_ManyViews(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	queries := make([]models.AssetQuery, 150)
	for i := range queries {
		queries[i] = fx.query(models.AssetQuery{Kind: anyOf("pdf")})
	}
	got, err := fx.repo.CountViews(fx.ctx, queries)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 150 || got[149].Total != 1 {
		t.Errorf("got %d counts, last %+v; want 150 counting the one pdf", len(got), got[len(got)-1])
	}
}

func TestAssetCountViews_Edges(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	if got, err := fx.repo.CountViews(fx.ctx, nil); err != nil || got != nil {
		t.Errorf("no views = %v, %v; want nil without a query", got, err)
	}
	scoped := fx.query(models.AssetQuery{ScopeCampaign: "c-explicit"})
	if _, err := fx.repo.CountViews(fx.ctx, []models.AssetQuery{fx.query(models.AssetQuery{}), scoped}); !errors.Is(err, repository.ErrMixedViewScope) {
		t.Errorf("mixed scopes: err = %v, want ErrMixedViewScope", err)
	}
	got, err := fx.repo.CountViews(fx.ctx, []models.AssetQuery{scoped})
	if err != nil || len(got) != 1 || got[0].Total != 2 {
		t.Errorf("scoped view = %v, %v; want the campaign's 2 assets", got, err)
	}
}
