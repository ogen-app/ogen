package restore_test

import (
	"context"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/post_actions/restore"
)

// Restoring a body onto a planned post makes it a draft and logs that
// transition next to post_restored.
func TestRestoreLogsThePlannedToDraftTransition(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	// One connection with FKs off, so posts can be seeded without a campaign.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec("SET session_replication_role = replica"); err != nil {
		t.Fatalf("disable fks: %v", err)
	}
	ctx := tenantctx.With(context.Background(), models.DefaultTenantID)
	posts := repository.NewPostRepository(db)
	versions := repository.NewPostVersionRepository(db)
	logs := repository.NewPostLogRepository(db)

	id, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	post := &models.Post{
		ID: id, CampaignID: "camp-1", Title: "t",
		MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
		Status: models.PostStatusPlanned, CTAType: models.CTATypeNone, CreatedBy: "user-1",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := posts.Create(ctx, post); err != nil {
		t.Fatalf("create: %v", err)
	}
	versionID, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := versions.CreateNext(ctx, &models.PostVersion{ID: versionID, PostID: id, Content: "An old body.", Creator: "user"}); err != nil {
		t.Fatalf("seed version: %v", err)
	}

	svc := restore.New(db, posts, versions, logs, nil)
	res, err := svc.Restore(ctx, id, restore.Options{Actor: "user-1", Trigger: restore.TriggerAPI, VersionNumber: 1})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Post.Status != models.PostStatusDraft {
		t.Fatalf("status = %s, want draft", res.Post.Status)
	}

	entries, err := logs.ListByPostID(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var transition *models.PostLog
	for i := range entries {
		if entries[i].EventType == models.PostLogEventStateTransition {
			transition = &entries[i]
		}
	}
	if transition == nil || transition.FromStatus == nil || transition.ToStatus == nil ||
		*transition.FromStatus != models.PostStatusPlanned || *transition.ToStatus != models.PostStatusDraft {
		t.Fatalf("no planned -> draft state_transition among %+v", entries)
	}
}

// stalePosts reads every post as still planned, as a restore that loaded it
// just before another restore drafted it would.
type stalePosts struct {
	repository.PostRepository
}

func (r stalePosts) GetByID(ctx context.Context, id string) (*models.Post, error) {
	p, err := r.PostRepository.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	p.Status = models.PostStatusPlanned
	return p, nil
}

// The transition is worked out from the status under the row lock, so a
// restore that read the post before another one drafted it logs none.
func TestRestoreReadsTheStatusUnderTheLock(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec("SET session_replication_role = replica"); err != nil {
		t.Fatalf("disable fks: %v", err)
	}
	ctx := tenantctx.With(context.Background(), models.DefaultTenantID)
	posts := repository.NewPostRepository(db)
	versions := repository.NewPostVersionRepository(db)
	logs := repository.NewPostLogRepository(db)

	id, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := posts.Create(ctx, &models.Post{
		ID: id, CampaignID: "camp-1", Title: "t", Content: "The current body.",
		MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
		Status: models.PostStatusDraft, CTAType: models.CTATypeNone, CreatedBy: "user-1",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	versionID, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := versions.CreateNext(ctx, &models.PostVersion{ID: versionID, PostID: id, Content: "An old body.", Creator: "user"}); err != nil {
		t.Fatalf("seed version: %v", err)
	}

	svc := restore.New(db, stalePosts{posts}, versions, logs, nil)
	res, err := svc.Restore(ctx, id, restore.Options{Actor: "user-1", Trigger: restore.TriggerAPI, VersionNumber: 1})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Post.Status != models.PostStatusDraft {
		t.Fatalf("status = %s, want draft", res.Post.Status)
	}
	entries, err := logs.ListByPostID(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.EventType == models.PostLogEventStateTransition {
			t.Fatalf("logged a transition %v -> %v for a post that was already a draft", *e.FromStatus, *e.ToStatus)
		}
	}
}
