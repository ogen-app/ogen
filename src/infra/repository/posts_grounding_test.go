package repository_test

import (
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// grounding_mode round-trips both labels and NULL, and the column refuses
// any other value.
func TestPostGroundingMode(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostRepository(db)

	for _, mode := range []*models.GroundingMode{models.GroundingFor(1), models.GroundingFor(0), nil} {
		post := seedSubmissionPost(t, repo, "camp-1", models.PostStatusDraft, "")
		post.GroundingMode = mode
		if err := repo.Update(ctx, post); err != nil {
			t.Fatalf("update grounding %v: %v", mode, err)
		}
		got, err := repo.GetByID(ctx, post.ID)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		switch {
		case mode == nil && got.GroundingMode != nil:
			t.Errorf("grounding = %q, want NULL", *got.GroundingMode)
		case mode != nil && (got.GroundingMode == nil || *got.GroundingMode != *mode):
			t.Errorf("grounding = %v, want %q", got.GroundingMode, *mode)
		}
	}

	post := seedSubmissionPost(t, repo, "camp-1", models.PostStatusDraft, "")
	post.GroundingMode = new(models.GroundingMode("speculative"))
	err := repo.Update(ctx, post)
	if err == nil || !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("unsupported grounding saved: err = %v", err)
	}
}
