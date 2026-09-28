package verify

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type fakePosts struct {
	repository.PostRepository
	err     error
	updates int
}

func (f *fakePosts) Update(context.Context, *models.Post, ...string) error {
	f.updates++
	return f.err
}

type fakeVersions struct {
	repository.PostVersionRepository
	latest  *models.PostVersion
	created []*models.PostVersion
}

func (f *fakeVersions) GetLatestByPostID(context.Context, string) (*models.PostVersion, error) {
	return f.latest, nil
}

func (f *fakeVersions) CreateNext(_ context.Context, v *models.PostVersion) error {
	f.created = append(f.created, v)
	return nil
}

func TestConfirmBackfillsAndSnapshots(t *testing.T) {
	versions := &fakeVersions{}
	svc := &Service{Posts: &fakePosts{}, Versions: versions}
	post := &models.Post{ID: "p", Content: "body", PublishedURL: "https://pasted"}
	ext := &zernio.ExternalPost{PlatformPostID: "ext-1", PlatformPostURL: "https://canonical"}

	if err := svc.Confirm(t.Context(), post, ext); err != nil {
		t.Fatal(err)
	}
	if post.PublisherPostID != "ext-1" || post.Publisher != models.PublisherZernio ||
		post.PublishedURL != "https://canonical" || post.Status != models.PostStatusPublished {
		t.Fatalf("post = %+v", post)
	}
	if len(versions.created) != 1 || versions.created[0].Note != models.PostVersionNotePublished {
		t.Fatalf("snapshots = %+v", versions.created)
	}

	// Re-verifying identical content adds no second snapshot.
	versions.latest = versions.created[0]
	if err := svc.Confirm(t.Context(), post, ext); err != nil {
		t.Fatal(err)
	}
	if len(versions.created) != 1 {
		t.Fatalf("duplicate snapshot: %d", len(versions.created))
	}
}

func TestConfirmKeepsExistingLinkage(t *testing.T) {
	post := &models.Post{ID: "p", PublisherPostID: "orig", Publisher: "other"}
	svc := &Service{Posts: &fakePosts{}}
	if err := svc.Confirm(t.Context(), post, &zernio.ExternalPost{PlatformPostID: "ext-1"}); err != nil {
		t.Fatal(err)
	}
	if post.PublisherPostID != "orig" || post.Publisher != "other" {
		t.Fatalf("linkage overwritten: %+v", post)
	}
}

func TestConfirmUpdateFailure(t *testing.T) {
	boom := errors.New("boom")
	versions := &fakeVersions{}
	svc := &Service{Posts: &fakePosts{err: boom}, Versions: versions}
	if err := svc.Confirm(t.Context(), &models.Post{ID: "p"}, &zernio.ExternalPost{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if len(versions.created) != 0 {
		t.Fatal("no snapshot after a failed update")
	}
}

func TestBuildCurrentUsesExternalIDAndPlatformName(t *testing.T) {
	post := &models.Post{ID: "p", Title: "T", PublisherPostID: "orig", Platform: &models.Platform{Name: "LinkedIn"}}
	ext := &zernio.ExternalPost{Platform: "linkedin", PlatformPostID: "ext-1", Analytics: zernio.ExternalPostAnalytics{Likes: 3}}
	got := buildCurrent(post, ext)
	if got.PublisherPostID != "ext-1" || got.Platform != "LinkedIn" || got.Likes != 3 || got.SyncStatus != "synced" {
		t.Fatalf("built = %+v", got)
	}
}
