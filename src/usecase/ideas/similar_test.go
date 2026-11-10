package ideas

import (
	"context"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// similarRepo serves one idea by id and the candidates to every query.
type similarRepo struct {
	repository.IdeaRepository
	ideas []models.Idea
	q     models.IdeaQuery
}

func (r *similarRepo) GetByID(_ context.Context, id string) (*models.Idea, error) {
	for _, i := range r.ideas {
		if i.ID == id {
			return &i, nil
		}
	}
	return nil, errNoRows
}

func (r *similarRepo) Query(_ context.Context, q models.IdeaQuery) (*models.IdeaQueryResult, error) {
	r.q = q
	return &models.IdeaQueryResult{Ideas: r.ideas}, nil
}

// Similar ideas share words or a source; the same source lifts a weak word
// match over the bar, and ideas with nothing but a campaign in common are
// left out. The idea itself and turned-down ideas never come back.
func TestSimilar(t *testing.T) {
	c1 := "c1"
	repo := &similarRepo{ideas: []models.Idea{
		{ID: "i1", Title: "Pricing myths founders believe", SourceAssetID: new("a1"), CampaignID: &c1},
		{ID: "i2", Title: "Five pricing myths, busted"},
		{ID: "i3", Title: "Founder story", Note: "how we started", SourceAssetID: new("a1")},
		{ID: "i4", Title: "Spa opening teaser", CampaignID: &c1},
		{ID: "i5", Title: "Pricing pages that convert", AssetIDs: []string{"a1"}},
	}}
	got, err := New(repo, nil).Similar(t.Context(), "i1", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(repo.q.Pile.Not, []string{models.IdeaPileNo}) {
		t.Fatalf("query = %+v; want turned-down ideas left out", repo.q)
	}
	var ids []string
	for _, s := range got {
		ids = append(ids, s.Idea.ID)
	}
	if !slices.Equal(ids, []string{"i2", "i3", "i5"}) {
		t.Fatalf("similar = %v", ids)
	}
	if !slices.Equal(got[0].Terms, []string{"myth", "pricing"}) || !got[1].SameSource || !slices.Equal(got[2].SharedAssets, []string{"a1"}) {
		t.Fatalf("why = %+v", got)
	}
	if got, _ := New(repo, nil).Similar(t.Context(), "i1", "", 1); len(got) != 1 {
		t.Fatalf("limited = %d", len(got))
	}
	if _, err := New(repo, nil).Similar(t.Context(), "gone", "", 5); err == nil {
		t.Fatal("an unknown idea must fail")
	}
}
