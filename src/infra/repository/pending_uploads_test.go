package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
)

// openPendingDB returns a fresh database with foreign keys enforced and a
// multi-connection pool, so the tests see the post_id SET NULL and real row
// locks. Fixtures go in through seed, which bypasses foreign keys.
func openPendingDB(t *testing.T) *bun.DB {
	t.Helper()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seed runs fn on one connection with foreign-key triggers off, so a fixture
// needs no campaign or user graph behind it.
func seed(t *testing.T, db *bun.DB, fn func(ctx context.Context, idb bun.IDB) error) {
	t.Helper()
	ctx := t.Context()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("seed conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "SET session_replication_role = replica"); err != nil {
		t.Fatalf("disable fks: %v", err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "SET session_replication_role = origin") }()
	if err := fn(ctx, conn); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func seedPendingPost(t *testing.T, db *bun.DB, id string) {
	t.Helper()
	seed(t, db, func(ctx context.Context, idb bun.IDB) error {
		_, err := idb.NewInsert().Model(&models.Post{
			ID: id, CampaignID: "camp-1", Title: id, MediaURLs: models.StringSlice{},
			UsedAssetIDs: models.StringSlice{}, Status: models.PostStatusDraft,
			CTAType: models.CTATypeNone, CreatedBy: "user-1",
		}).Exec(tenantCtx())
		if err != nil {
			return err
		}
		// Attachments reference their creator with a foreign key the tests keep on.
		_, err = idb.ExecContext(ctx, `INSERT INTO users (id, name, email, tenant_id, account_id)
			VALUES ('user-1', 'User', 'user-1@example.com', ?, 'account-1') ON CONFLICT DO NOTHING`, models.DefaultTenantID)
		return err
	})
}

func pendingUpload(id, postID, key string, expiresAt time.Time) *models.PendingUpload {
	return &models.PendingUpload{ID: id, PostID: &postID, S3Key: key, SizeBytes: 10, ExpiresAt: expiresAt}
}

func countPending(t *testing.T, db *bun.DB, key string) int {
	t.Helper()
	var n int
	if err := db.NewRaw(`SELECT count(*) FROM pending_uploads WHERE s3_key = ?`, key).Scan(t.Context(), &n); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	return n
}

func TestPendingUploadCreateStampsTenantAndKeyIsUnique(t *testing.T) {
	db := openPendingDB(t)
	repo := repository.NewPendingUploadRepository(db)
	seedPendingPost(t, db, "post-1")
	ctx := tenantCtx()

	exp := time.Now().Add(30 * time.Minute)
	if err := repo.Create(ctx, pendingUpload("pu-1", "post-1", "k1", exp)); err != nil {
		t.Fatalf("create: %v", err)
	}
	var tenant string
	if err := db.NewRaw(`SELECT tenant_id FROM pending_uploads WHERE id = 'pu-1'`).Scan(ctx, &tenant); err != nil {
		t.Fatalf("read tenant: %v", err)
	}
	if tenant != models.DefaultTenantID {
		t.Fatalf("tenant_id = %q, want %q", tenant, models.DefaultTenantID)
	}
	if err := repo.Create(ctx, pendingUpload("pu-2", "post-1", "k1", exp)); err == nil {
		t.Fatal("a second record for the same key must violate the unique key")
	}
}

func TestPendingUploadDeleteByKeyIsTenantScoped(t *testing.T) {
	db := openPendingDB(t)
	repo := repository.NewPendingUploadRepository(db)
	seedPendingPost(t, db, "post-1")
	if err := repo.Create(tenantCtx(), pendingUpload("pu-1", "post-1", "k1", time.Now())); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := repo.DeleteByKey(tenantctx.With(t.Context(), "tenant-2"), "k1"); err != nil {
		t.Fatalf("delete from other tenant: %v", err)
	}
	if countPending(t, db, "k1") != 1 {
		t.Fatal("another tenant must not delete the record")
	}
	if err := repo.DeleteByKey(tenantCtx(), "k1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if countPending(t, db, "k1") != 0 {
		t.Fatal("record should be gone")
	}
}

func TestPendingUploadListExpired(t *testing.T) {
	db := openPendingDB(t)
	repo := repository.NewPendingUploadRepository(db)
	now := time.Now().UTC()
	seed(t, db, func(ctx context.Context, idb bun.IDB) error {
		rows := []*models.PendingUpload{
			{ID: "old", S3Key: "k-old", SizeBytes: 1, ExpiresAt: now.Add(-3 * time.Hour)},
			{ID: "older", S3Key: "k-older", SizeBytes: 1, ExpiresAt: now.Add(-5 * time.Hour)},
			{ID: "fresh", S3Key: "k-fresh", SizeBytes: 1, ExpiresAt: now.Add(-time.Hour)},
		}
		for _, r := range rows {
			if _, err := idb.NewInsert().Model(r).Exec(tenantCtx()); err != nil {
				return err
			}
		}
		other := &models.PendingUpload{ID: "other", S3Key: "k-other", SizeBytes: 1, ExpiresAt: now.Add(-4 * time.Hour)}
		_, err := idb.NewInsert().Model(other).Exec(tenantctx.With(ctx, "tenant-2"))
		return err
	})
	cutoff := now.Add(-2 * time.Hour)

	got, err := repo.ListExpired(tenantctx.WithSystem(t.Context()), cutoff, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if ids := pendingIDs(got); !slices.Equal(ids, []string{"older", "other", "old"}) {
		t.Fatalf("system listing = %v, want every tenant's expired records oldest first", ids)
	}

	got, err = repo.ListExpired(tenantCtx(), cutoff, 1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if ids := pendingIDs(got); !slices.Equal(ids, []string{"older"}) {
		t.Fatalf("tenant listing with limit 1 = %v, want [older]", ids)
	}
}

func TestPendingUploadReap(t *testing.T) {
	now := time.Now().UTC()
	cutoff := now.Add(-2 * time.Hour)
	expired := now.Add(-3 * time.Hour)

	setup := func(t *testing.T) (*bun.DB, repository.PendingUploadRepository) {
		db := openPendingDB(t)
		seedPendingPost(t, db, "post-1")
		repo := repository.NewPendingUploadRepository(db)
		if err := repo.Create(tenantCtx(), pendingUpload("pu-1", "post-1", "k1", expired)); err != nil {
			t.Fatalf("create: %v", err)
		}
		return db, repo
	}
	sys := func(t *testing.T) context.Context { return tenantctx.WithSystem(t.Context()) }

	t.Run("abandoned upload: object removed, record dropped", func(t *testing.T) {
		db, repo := setup(t)
		var removed []string
		out, err := repo.Reap(sys(t), "pu-1", cutoff, func(_ context.Context, u models.PendingUpload) error {
			removed = append(removed, u.S3Key)
			return nil
		})
		if err != nil || out != repository.ReapRemoved {
			t.Fatalf("reap = %v, %v; want ReapRemoved", out, err)
		}
		if !slices.Equal(removed, []string{"k1"}) || countPending(t, db, "k1") != 0 {
			t.Fatalf("removed %v, record count %d", removed, countPending(t, db, "k1"))
		}
	})

	t.Run("attached key: object kept, record dropped", func(t *testing.T) {
		db, repo := setup(t)
		att := &models.PostAttachment{ID: "att-1", PostID: "post-1", MimeType: "video/mp4", SizeBytes: 10, S3Key: "k1", CreatedBy: "user-1"}
		if err := repository.NewPostAttachmentRepository(db).CreateAtNextPosition(tenantCtx(), att); err != nil {
			t.Fatalf("attach: %v", err)
		}
		out, err := repo.Reap(sys(t), "pu-1", cutoff, func(context.Context, models.PendingUpload) error {
			t.Fatal("an attached object must never be removed")
			return nil
		})
		if err != nil || out != repository.ReapAttached {
			t.Fatalf("reap = %v, %v; want ReapAttached", out, err)
		}
		if countPending(t, db, "k1") != 0 {
			t.Fatal("record should be dropped")
		}
	})

	t.Run("remove error keeps the record", func(t *testing.T) {
		db, repo := setup(t)
		boom := errors.New("storage down")
		_, err := repo.Reap(sys(t), "pu-1", cutoff, func(context.Context, models.PendingUpload) error { return boom })
		if !errors.Is(err, boom) {
			t.Fatalf("reap err = %v, want %v", err, boom)
		}
		if countPending(t, db, "k1") != 1 {
			t.Fatal("record must survive a failed remove so the next sweep retries")
		}
	})

	t.Run("record inside the grace period is skipped", func(t *testing.T) {
		db, repo := setup(t)
		out, err := repo.Reap(sys(t), "pu-1", expired.Add(-time.Minute), func(context.Context, models.PendingUpload) error {
			t.Fatal("must not remove before the cutoff")
			return nil
		})
		if err != nil || out != repository.ReapSkipped || countPending(t, db, "k1") != 1 {
			t.Fatalf("reap = %v, %v; want ReapSkipped with the record kept", out, err)
		}
	})

	t.Run("record locked by a finalize is skipped, not waited on", func(t *testing.T) {
		db, repo := setup(t)
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(t.Context(), `SELECT 1 FROM pending_uploads WHERE id = 'pu-1' FOR UPDATE`); err != nil {
			t.Fatalf("lock: %v", err)
		}
		ctx, cancel := context.WithTimeout(sys(t), 5*time.Second)
		defer cancel()
		out, err := repo.Reap(ctx, "pu-1", cutoff, func(context.Context, models.PendingUpload) error {
			t.Fatal("a locked record must not be removed")
			return nil
		})
		if err != nil || out != repository.ReapSkipped {
			t.Fatalf("reap = %v, %v; want ReapSkipped", out, err)
		}
	})
}

func TestCreateFromPendingUpload(t *testing.T) {
	db := openPendingDB(t)
	seedPendingPost(t, db, "post-1")
	pending := repository.NewPendingUploadRepository(db)
	atts := repository.NewPostAttachmentRepository(db)
	ctx := tenantCtx()
	if err := pending.Create(ctx, pendingUpload("pu-1", "post-1", "k1", time.Now())); err != nil {
		t.Fatalf("create pending: %v", err)
	}
	newAtt := func(id, key string) *models.PostAttachment {
		return &models.PostAttachment{ID: id, PostID: "post-1", MimeType: "video/mp4", SizeBytes: 10, S3Key: key, CreatedBy: "user-1"}
	}

	if err := atts.CreateFromPendingUpload(ctx, newAtt("att-1", "k1")); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if countPending(t, db, "k1") != 0 {
		t.Fatal("finalize must consume the record")
	}
	if err := atts.CreateFromPendingUpload(ctx, newAtt("att-2", "k1")); !errors.Is(err, repository.ErrUploadAlreadyAttached) {
		t.Fatalf("repeat finalize err = %v, want ErrUploadAlreadyAttached", err)
	}
	if err := atts.CreateFromPendingUpload(ctx, newAtt("att-3", "k-swept")); !errors.Is(err, repository.ErrPendingUploadGone) {
		t.Fatalf("finalize without a record err = %v, want ErrPendingUploadGone", err)
	}
	for _, id := range []string{"att-2", "att-3"} {
		if _, err := atts.GetByID(ctx, id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s must not be inserted, got %v", id, err)
		}
	}
}

// TestCreateFromPendingUploadWaitsForSweep pins the race: a finalize that
// arrives while the sweep holds the record waits for it and, once the sweep
// commits, fails as expired instead of attaching a deleted object.
func TestCreateFromPendingUploadWaitsForSweep(t *testing.T) {
	db := openPendingDB(t)
	seedPendingPost(t, db, "post-1")
	pending := repository.NewPendingUploadRepository(db)
	atts := repository.NewPostAttachmentRepository(db)
	if err := pending.Create(tenantCtx(), pendingUpload("pu-1", "post-1", "k1", time.Now().Add(-3*time.Hour))); err != nil {
		t.Fatalf("create pending: %v", err)
	}

	removing := make(chan struct{})
	release := make(chan struct{})
	reaped := make(chan error, 1)
	go func() {
		_, err := pending.Reap(tenantctx.WithSystem(context.Background()), "pu-1", time.Now(), func(context.Context, models.PendingUpload) error {
			close(removing)
			<-release
			return nil
		})
		reaped <- err
	}()
	<-removing

	finalized := make(chan error, 1)
	go func() {
		finalized <- atts.CreateFromPendingUpload(tenantCtx(), &models.PostAttachment{
			ID: "att-1", PostID: "post-1", MimeType: "video/mp4", SizeBytes: 10, S3Key: "k1", CreatedBy: "user-1",
		})
	}()
	select {
	case err := <-finalized:
		t.Fatalf("finalize must wait for the sweep's lock, returned %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-reaped; err != nil {
		t.Fatalf("reap: %v", err)
	}
	if err := <-finalized; !errors.Is(err, repository.ErrPendingUploadGone) {
		t.Fatalf("finalize err = %v, want ErrPendingUploadGone", err)
	}
}

func TestPendingUploadSurvivesPostDelete(t *testing.T) {
	db := openPendingDB(t)
	seedPendingPost(t, db, "post-1")
	repo := repository.NewPendingUploadRepository(db)
	if err := repo.Create(tenantCtx(), pendingUpload("pu-1", "post-1", "k1", time.Now())); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM posts WHERE id = 'post-1'`); err != nil {
		t.Fatalf("delete post: %v", err)
	}
	var postID sql.NullString
	if err := db.NewRaw(`SELECT post_id FROM pending_uploads WHERE id = 'pu-1'`).Scan(t.Context(), &postID); err != nil {
		t.Fatalf("record must survive the post: %v", err)
	}
	if postID.Valid {
		t.Fatalf("post_id = %q, want NULL", postID.String)
	}
}

func pendingIDs(rows []models.PendingUpload) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}
