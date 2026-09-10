package post_quality

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

// fakeVersionRepo is a minimal repository.PostVersionRepository for the
// content-resolution tests: only GetLatestByPostID is exercised.
type fakeVersionRepo struct {
	latest    *models.PostVersion
	err       error
	gotPostID string
}

func (f *fakeVersionRepo) GetLatestByPostID(_ context.Context, postID string) (*models.PostVersion, error) {
	f.gotPostID = postID
	return f.latest, f.err
}

func (f *fakeVersionRepo) Create(context.Context, *models.PostVersion) error { return nil }
func (f *fakeVersionRepo) ListByPostID(context.Context, string) ([]models.PostVersion, error) {
	return nil, nil
}
func (f *fakeVersionRepo) GetByPostIDAndVersionNumber(context.Context, string, int) (*models.PostVersion, error) {
	return nil, nil
}
func (f *fakeVersionRepo) CountByPostID(context.Context, string) (int, error) { return 0, nil }

// resolveAssessedContent is the CON-184 seam that makes the assess flow score
// the latest committed version snapshot instead of the live editor HEAD
// (posts.content). These tests pin the three branches that decide which
// content the model ultimately sees.
func TestResolveAssessedContent(t *testing.T) {
	const head = "live HEAD content (uncommitted keystrokes)"

	t.Run("uses the latest committed version over the live HEAD", func(t *testing.T) {
		post := &models.Post{ID: "post-1", Content: head}
		repo := &fakeVersionRepo{latest: &models.PostVersion{VersionNumber: 3, Content: "committed v3"}}

		if err := resolveAssessedContent(t.Context(), PostQualityRepos{Versions: repo}, post); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if post.Content != "committed v3" {
			t.Fatalf("Content = %q, want the latest version's content", post.Content)
		}
		if repo.gotPostID != "post-1" {
			t.Fatalf("looked up version for %q, want the post's own id", repo.gotPostID)
		}
	})

	t.Run("falls back to posts.content when the post has no saved version", func(t *testing.T) {
		// GetLatestByPostID returns (nil, nil) for a post that was never
		// snapshotted — a brand-new post must still be assessable.
		post := &models.Post{ID: "post-2", Content: head}
		repo := &fakeVersionRepo{latest: nil}

		if err := resolveAssessedContent(t.Context(), PostQualityRepos{Versions: repo}, post); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if post.Content != head {
			t.Fatalf("Content = %q, want the live HEAD kept as-is", post.Content)
		}
	})

	t.Run("is a no-op when version resolution is disabled", func(t *testing.T) {
		// Nil Versions repo preserves the pre-CON-184 behaviour of scoring
		// posts.content directly.
		post := &models.Post{ID: "post-3", Content: head}

		if err := resolveAssessedContent(t.Context(), PostQualityRepos{Versions: nil}, post); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if post.Content != head {
			t.Fatalf("Content = %q, want it untouched when Versions is nil", post.Content)
		}
	})

	t.Run("propagates a version lookup failure", func(t *testing.T) {
		post := &models.Post{ID: "post-4", Content: head}
		wantErr := errors.New("db down")
		repo := &fakeVersionRepo{err: wantErr}

		err := resolveAssessedContent(t.Context(), PostQualityRepos{Versions: repo}, post)
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want it to wrap %v", err, wantErr)
		}
		if post.Content != head {
			t.Fatalf("Content = %q, want it untouched on error", post.Content)
		}
	})
}
