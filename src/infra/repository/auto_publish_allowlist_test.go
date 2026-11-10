package repository_test

import (
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/infra/repository"
)

// ReplaceIf replaces the list only while it is what the caller read, in any
// order.
func TestAutoPublishAllowlistReplaceIf(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewAutoPublishAllowlistRepository(db)
	if err := repo.Set(ctx, []string{"x", "linkedin"}); err != nil {
		t.Fatal(err)
	}
	ok, err := repo.ReplaceIf(ctx, nil, []string{"linkedin"}, []string{"instagram"})
	if err != nil || ok {
		t.Fatalf("stale replace = %v, %v", ok, err)
	}
	ok, err = repo.ReplaceIf(ctx, nil, []string{"linkedin", "x"}, []string{"x", "instagram"})
	if err != nil || !ok {
		t.Fatalf("replace = %v, %v", ok, err)
	}
	if got, _ := repo.List(ctx); !slices.Equal(got, []string{"instagram", "x"}) {
		t.Fatalf("list = %v", got)
	}
	if ok, err := repo.ReplaceIf(ctx, nil, []string{"instagram", "x"}, nil); err != nil || !ok {
		t.Fatalf("clear = %v, %v", ok, err)
	}
	if got, _ := repo.List(ctx); len(got) != 0 {
		t.Fatalf("cleared list = %v", got)
	}
}
