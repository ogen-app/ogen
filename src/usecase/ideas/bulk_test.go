package ideas

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// selectionRepo answers QueryIDs with a fixed match size and DeleteMany with
// the ids it holds.
type selectionRepo struct {
	repository.IdeaRepository
	total   int
	held    []string
	deleted []string
}

func (s *selectionRepo) QueryIDs(_ context.Context, _ models.IdeaQuery, limit int) ([]string, int, error) {
	return make([]string, min(s.total, limit)), s.total, nil
}

func (s *selectionRepo) DeleteMany(_ context.Context, ids []string) ([]string, error) {
	s.deleted = ids
	var out []string
	for _, id := range ids {
		if slices.Contains(s.held, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

func TestQueryIDsRefusesASelectionOverTheCap(t *testing.T) {
	svc := New(&selectionRepo{total: MaxSelection}, nil)
	if ids, total, err := svc.QueryIDs(t.Context(), models.IdeaQuery{}); err != nil || len(ids) != MaxSelection || total != MaxSelection {
		t.Fatalf("at the cap: %d ids, total %d, %v", len(ids), total, err)
	}
	svc = New(&selectionRepo{total: MaxSelection + 1}, nil)
	if _, total, err := svc.QueryIDs(t.Context(), models.IdeaQuery{}); !errors.Is(err, ErrTooMany) || total != MaxSelection+1 {
		t.Fatalf("over the cap: total %d, %v; want ErrTooMany with the total", total, err)
	}
}

func TestBulkDeleteReportsWhatItCouldNotDelete(t *testing.T) {
	repo := &selectionRepo{held: []string{"a", "c"}}
	deleted, failed, err := New(repo, nil).BulkDelete(t.Context(), []string{"c", "b", "a", "c", " "})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(repo.deleted, []string{"a", "b", "c"}) {
		t.Errorf("asked to delete %v, want the ids de-duplicated without blanks", repo.deleted)
	}
	if !slices.Equal(deleted, []string{"a", "c"}) || len(failed) != 1 || failed[0] != (BulkFailure{ID: "b", Code: BulkCodeNotFound}) {
		t.Errorf("deleted %v, failed %v", deleted, failed)
	}
	tooMany := make([]string, MaxBulkIDs+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprint(i)
	}
	for _, ids := range [][]string{nil, {" "}, tooMany} {
		if _, _, err := New(repo, nil).BulkDelete(t.Context(), ids); !errors.Is(err, ErrBulkSize) {
			t.Errorf("%d ids: err = %v, want ErrBulkSize", len(ids), err)
		}
	}
}
