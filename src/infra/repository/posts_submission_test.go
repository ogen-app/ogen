package repository_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func seedSubmissionPost(t *testing.T, repo repository.PostRepository, campaignID string, status models.PostStatus, publisherPostID string) *models.Post {
	t.Helper()
	id, err := models.NewID()
	if err != nil {
		t.Fatalf("mint id: %v", err)
	}
	now := time.Now().UTC()
	post := &models.Post{
		ID:              id,
		CampaignID:      campaignID,
		Title:           "t",
		Content:         "body",
		MediaURLs:       models.StringSlice{},
		UsedAssetIDs:    models.StringSlice{},
		Status:          status,
		CTAType:         models.CTATypeNone,
		CreatedBy:       "user-1",
		ScheduledAt:     &now,
		PublisherPostID: publisherPostID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := repo.Create(tenantCtx(), post); err != nil {
		t.Fatalf("create: %v", err)
	}
	return post
}

func TestPostUpdateSubmission(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostRepository(db)
	post := seedSubmissionPost(t, repo, "camp-1", models.PostStatusScheduled, "")

	// First submit: the post holds no submission yet.
	stale := *post
	stale.Content = "stale body the worker loaded"
	stale.PublisherPostID = "z-1"
	ok, err := repo.UpdateSubmission(ctx, &stale, "", "publisher_post_id")
	if err != nil || !ok {
		t.Fatalf("first write: ok=%v err=%v", ok, err)
	}
	got, _ := repo.GetByID(ctx, post.ID)
	if got.PublisherPostID != "z-1" || got.Content != "body" {
		t.Fatalf("got publisher_post_id=%q content=%q; only the named column may change", got.PublisherPostID, got.Content)
	}

	// A writer that loaded the post before z-1 landed no longer matches.
	if ok, _ := repo.UpdateSubmission(ctx, &stale, "", "publisher_post_id"); ok {
		t.Fatal("write against an outdated submission must not land")
	}

	// Once the post leaves scheduled, nothing holding z-1 may write either.
	got.Status = models.PostStatusReadyForPublish
	got.PublisherPostID = ""
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("unschedule: %v", err)
	}
	stale.Status = models.PostStatusScheduled
	if ok, _ := repo.UpdateSubmission(ctx, &stale, "z-1", "status", "publisher_post_id"); ok {
		t.Fatal("write on an unscheduled post must not land")
	}
	got, _ = repo.GetByID(ctx, post.ID)
	if got.Status != models.PostStatusReadyForPublish {
		t.Fatalf("status = %q, the unschedule was overwritten", got.Status)
	}
}

func TestPostListByPublisherPostIDs(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostRepository(db)
	a := seedSubmissionPost(t, repo, "camp-1", models.PostStatusScheduled, "z-a")
	seedSubmissionPost(t, repo, "camp-1", models.PostStatusScheduled, "z-b")

	got, err := repo.ListByPublisherPostIDs(ctx, []string{"z-a", "z-missing"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != a.ID || got[0].Status != models.PostStatusScheduled {
		t.Fatalf("got %+v, want only %s", got, a.ID)
	}
	if got, _ := repo.ListByPublisherPostIDs(ctx, nil); got != nil {
		t.Fatalf("empty ids: got %+v", got)
	}
}

func TestPostUnscheduleByCampaignTx(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostRepository(db)
	auto := seedSubmissionPost(t, repo, "camp-u", models.PostStatusScheduled, "z-1")
	manual := seedSubmissionPost(t, repo, "camp-u", models.PostStatusScheduledForManualPublish, "")
	draft := seedSubmissionPost(t, repo, "camp-u", models.PostStatusDraft, "")
	published := seedSubmissionPost(t, repo, "camp-u", models.PostStatusPublished, "z-pub")
	elsewhere := seedSubmissionPost(t, repo, "camp-other", models.PostStatusScheduled, "z-2")

	var before []models.Post
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		before, err = repo.UnscheduleByCampaignTx(ctx, tx, "camp-u")
		return err
	})
	if err != nil {
		t.Fatalf("unschedule: %v", err)
	}
	ids := make([]string, len(before))
	for i, p := range before {
		ids[i] = p.ID
		if p.ID == auto.ID && (p.PublisherPostID != "z-1" || p.Status != models.PostStatusScheduled) {
			t.Fatalf("returned row must be the pre-change state: %+v", p)
		}
	}
	slices.Sort(ids)
	want := []string{auto.ID, manual.ID}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("unscheduled %v, want %v", ids, want)
	}

	for _, tc := range []struct {
		post   *models.Post
		status models.PostStatus
		pubID  string
	}{
		{auto, models.PostStatusDraft, ""},
		{manual, models.PostStatusDraft, ""},
		{draft, models.PostStatusDraft, ""},
		{published, models.PostStatusPublished, "z-pub"},
		{elsewhere, models.PostStatusScheduled, "z-2"},
	} {
		got, err := repo.GetByID(ctx, tc.post.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != tc.status || got.PublisherPostID != tc.pubID {
			t.Errorf("post %s: status=%q publisher_post_id=%q, want %q/%q", tc.post.ID, got.Status, got.PublisherPostID, tc.status, tc.pubID)
		}
	}
}

func TestPostDeleteTxReturnsTheDeletedRow(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostRepository(db)
	post := seedSubmissionPost(t, repo, "camp-1", models.PostStatusScheduled, "z-1")

	row, err := repo.DeleteTx(ctx, db, post.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if row == nil || row.ID != post.ID || row.Status != models.PostStatusScheduled || row.PublisherPostID != "z-1" || row.TenantID == "" {
		t.Fatalf("returned row = %+v", row)
	}
	if row, err := repo.DeleteTx(ctx, db, post.ID); err != nil || row != nil {
		t.Fatalf("second delete: row=%+v err=%v, want nil, nil", row, err)
	}
}

func TestPostUpdateWhileScheduled(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostRepository(db)
	post := seedSubmissionPost(t, repo, "camp-1", models.PostStatusScheduled, "z-1")

	edit := *post
	edit.Title = "edited"
	if ok, err := repo.UpdateWhileScheduled(ctx, &edit, "z-1", "used_asset_ids"); err != nil || !ok {
		t.Fatalf("edit of a scheduled post: ok=%v err=%v", ok, err)
	}

	// A cancel lands: the post leaves scheduled and drops its Zernio id.
	cancelled, _ := repo.GetByID(ctx, post.ID)
	cancelled.Status = models.PostStatusReadyForPublish
	cancelled.PublisherPostID = ""
	if err := repo.Update(ctx, cancelled); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	stale := edit
	stale.Title = "stale edit"
	if ok, _ := repo.UpdateWhileScheduled(ctx, &stale, "z-1"); ok {
		t.Fatal("a stale edit must not restore a cancelled post")
	}
	got, _ := repo.GetByID(ctx, post.ID)
	if got.Status != models.PostStatusReadyForPublish || got.Title != "edited" {
		t.Fatalf("status=%q title=%q", got.Status, got.Title)
	}
}
