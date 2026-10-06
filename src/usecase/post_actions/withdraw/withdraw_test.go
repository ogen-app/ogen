package withdraw_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/post_actions/withdraw"
)

type recorder struct {
	tasks []queues.WithdrawZernioPostTask
}

func (r *recorder) EnqueueWithdrawTx(_ context.Context, _ *sql.Tx, task queues.WithdrawZernioPostTask) error {
	r.tasks = append(r.tasks, task)
	return nil
}

func seed(t *testing.T, ctx context.Context, posts repository.PostRepository, status models.PostStatus, publisherPostID string) *models.Post {
	t.Helper()
	id, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	p := &models.Post{
		ID: id, CampaignID: "camp-1", Title: "t", Content: "body",
		MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
		Status: status, CTAType: models.CTATypeNone, CreatedBy: "user-1",
		PublisherPostID: publisherPostID, CreatedAt: now, UpdatedAt: now,
	}
	if err := posts.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	return p
}

// DeletePost decides on the row it deleted, not on the caller's earlier read.
func TestDeletePostDecidesOnTheDeletedRow(t *testing.T) {
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

	t.Run("scheduled after the caller loaded it", func(t *testing.T) {
		jobs := &recorder{}
		svc := withdraw.New(db, posts, nil, nil, jobs)
		loaded := seed(t, ctx, posts, models.PostStatusDraft, "")
		// A submit lands between the caller's read and the delete.
		live := *loaded
		live.Status = models.PostStatusScheduled
		live.PublisherPostID = "z-late"
		if err := posts.Update(ctx, &live); err != nil {
			t.Fatal(err)
		}

		deleted, err := svc.DeletePost(ctx, loaded, "user-1")
		if err != nil || !deleted {
			t.Fatalf("deleted=%v err=%v", deleted, err)
		}
		if len(jobs.tasks) != 1 || jobs.tasks[0].PublisherPostID != "z-late" {
			t.Fatalf("withdrawals = %+v, want one for z-late", jobs.tasks)
		}
	})

	t.Run("published after the caller loaded it", func(t *testing.T) {
		jobs := &recorder{}
		svc := withdraw.New(db, posts, nil, nil, jobs)
		loaded := seed(t, ctx, posts, models.PostStatusScheduled, "z-pub")
		live := *loaded
		live.Status = models.PostStatusPublished
		if err := posts.Update(ctx, &live); err != nil {
			t.Fatal(err)
		}

		deleted, err := svc.DeletePost(ctx, loaded, "user-1")
		if !errors.Is(err, withdraw.ErrPublished) || deleted {
			t.Fatalf("deleted=%v err=%v, want ErrPublished", deleted, err)
		}
		if _, err := posts.GetByID(ctx, loaded.ID); err != nil {
			t.Fatalf("the published post must survive: %v", err)
		}
		if len(jobs.tasks) != 0 {
			t.Fatalf("withdrawals = %+v, want none", jobs.tasks)
		}
	})
}
