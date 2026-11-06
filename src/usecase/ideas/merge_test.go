package ideas

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// mergeRepo holds ideas in memory and applies a merge as the repository
// does, inside its plan.
type mergeRepo struct {
	repository.IdeaRepository
	byID     map[string]models.Idea
	compiled []string
	columns  []string
}

func (r *mergeRepo) MergeSet(_ context.Context, ids []string) (repository.IdeaMergeSet, error) {
	set := repository.IdeaMergeSet{}
	for _, id := range ids {
		if i, ok := r.byID[id]; ok {
			set.Ideas = append(set.Ideas, i)
		}
		if slices.Contains(r.compiled, id) {
			set.Compiled = append(set.Compiled, id)
		}
	}
	return set, nil
}

func (r *mergeRepo) MergeIdeas(ctx context.Context, ids []string, plan func(repository.IdeaMergeSet) (repository.IdeaMerge, error)) error {
	set, _ := r.MergeSet(ctx, ids)
	m, err := plan(set)
	if err != nil {
		return err
	}
	r.byID[m.Keep.ID] = *m.Keep
	r.columns = m.Columns
	for _, id := range m.Delete {
		delete(r.byID, id)
	}
	return nil
}

func (r *mergeRepo) GetByID(_ context.Context, id string) (*models.Idea, error) {
	if i, ok := r.byID[id]; ok {
		return &i, nil
	}
	return nil, errNoRows
}

func TestMerge(t *testing.T) {
	grounded, exploratory := models.GroundingGrounded, models.GroundingExploratory
	c1, c2 := "c1", "c2"
	repo := &mergeRepo{byID: map[string]models.Idea{
		"keep": {ID: "keep", Title: "Pricing myths", Note: "Bust three myths.", CampaignID: &c1, AssetIDs: []string{"a1"}, GroundingMode: &exploratory},
		"dup1": {ID: "dup1", Title: "5 pricing myths", Note: "bust  THREE myths.", CampaignID: &c1, AssetIDs: []string{"a2", "a1"}},
		"dup2": {ID: "dup2", Title: "Pricing myths again", Note: "Use the CFO survey.", CampaignID: &c1, AssetIDs: []string{"a3"}, GroundingMode: &grounded},
		"else": {ID: "else", Title: "Pricing myths", CampaignID: &c2},
		"free": {ID: "free", Title: "Pricing myths"},
	}}
	svc := New(repo, nil)
	ctx := t.Context()

	for _, tc := range []struct {
		keep  string
		merge []string
		want  error
	}{
		{"keep", nil, ErrMergeSize},
		{"keep", make([]string, MaxMergeIDs+1), ErrMergeSize},
		{"keep", []string{"dup1", "keep"}, ErrMergeSelf},
		{"keep", []string{"dup1", "gone"}, ErrNotFound},
		{"gone", []string{"dup1"}, ErrNotFound},
		{"keep", []string{"else"}, ErrMergeScope},
		{"keep", []string{"free"}, ErrMergeScope},
		{"free", []string{"keep"}, ErrMergeScope},
	} {
		if _, err := svc.Merge(ctx, tc.keep, tc.merge); !errors.Is(err, tc.want) {
			t.Errorf("merge %s <- %v = %v, want %v", tc.keep, tc.merge, err, tc.want)
		}
	}
	if !IsValidation(ErrMergeScope) || !IsValidation(ErrMergeCompiled) || len(repo.byID) != 5 {
		t.Fatal("a refused merge changed something, or its error is not a 400")
	}

	repo.compiled = []string{"dup2"}
	if _, err := svc.CheckMerge(ctx, "keep", []string{"dup1", "dup2"}); !errors.Is(err, ErrMergeCompiled) {
		t.Fatalf("compiled = %v", err)
	}
	repo.compiled = nil
	if keep, err := svc.CheckMerge(ctx, "keep", []string{"dup1", "dup2"}); err != nil || keep.Title != "Pricing myths" || len(repo.byID) != 5 {
		t.Fatalf("check = %+v, %v; want the keep, nothing merged", keep, err)
	}

	res, err := svc.Merge(ctx, " keep ", []string{"dup1", "dup2", "dup1"})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Idea
	if !slices.Equal(res.Deleted, []string{"dup1", "dup2"}) || len(repo.byID) != 3 {
		t.Fatalf("deleted = %v, left %d", res.Deleted, len(repo.byID))
	}
	if got.Title != "Pricing myths" || !slices.Equal(got.AssetIDs, []string{"a1", "a2", "a3"}) || *got.GroundingMode != grounded {
		t.Fatalf("kept = %+v", got)
	}
	// dup1's note is already there (case and spacing aside); dup2's is added.
	if got.Note != "Bust three myths.\n\nUse the CFO survey." {
		t.Fatalf("note = %q", got.Note)
	}
	if !slices.Equal(repo.columns, []string{"asset_ids", "grounding_mode", "note"}) {
		t.Fatalf("columns = %v", repo.columns)
	}
}

func TestFoldInto_AnalysisNoteAndCap(t *testing.T) {
	keep := models.Idea{ID: "k", Origin: models.IdeaOriginAssetAnalysis, Note: "From the analysis."}
	if cols := foldInto(&keep, []models.Idea{{Note: "Another angle."}}); len(cols) != 0 || keep.Note != "From the analysis." {
		t.Fatalf("analysis keep = %q, %v; its note is its analysis's", keep.Note, cols)
	}
	long := strings.Repeat("x", MaxNoteLen-5)
	if got := appendNote(long, "too long to fit"); got != long {
		t.Fatal("a note past the cap was appended")
	}
	if got := appendNote("", " first "); got != "first" {
		t.Fatalf("empty keep note = %q", got)
	}
}
