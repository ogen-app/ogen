package repository_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// TestCreateAtNextPositionRequiresParentPost guards CON-97: the FOR UPDATE lock
// must confirm a matching parent post exists in the tenant. Attaching to a
// missing or cross-tenant post fails closed (sql.ErrNoRows) rather than
// inserting an orphaned attachment. openMigratedDB bypasses FK enforcement, so
// this is caught by the lock check itself, not the post_id foreign key.
func TestCreateAtNextPositionRequiresParentPost(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewPostAttachmentRepository(db)
	ctx := tenantCtx()

	seedPost(t, db, "post-x", "", "", time.Now().UTC())

	ok := &models.PostAttachment{
		ID: "att-1", PostID: "post-x", MimeType: "image/png",
		SizeBytes: 1, ChecksumSHA256: "a", S3Key: "k1", CreatedBy: "user-1",
	}
	if err := repo.CreateAtNextPosition(ctx, ok); err != nil {
		t.Fatalf("attaching to an existing in-tenant post should succeed: %v", err)
	}

	orphan := &models.PostAttachment{
		ID: "att-2", PostID: "does-not-exist", MimeType: "image/png",
		SizeBytes: 1, ChecksumSHA256: "b", S3Key: "k2", CreatedBy: "user-1",
	}
	if err := repo.CreateAtNextPosition(ctx, orphan); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("attaching to a missing/cross-tenant post must fail closed with ErrNoRows, got %v", err)
	}

	// post-x genuinely exists (it was just attached to above), but only in the
	// default tenant. A second tenant must not be able to attach to it: the
	// FOR UPDATE lock is scoped by tenant_id, so the lookup finds no row and
	// fails closed rather than inserting a cross-tenant attachment.
	// This is the case the orphan probe above can't reach — it proves the
	// tenant_id half of the lock predicate, not just post_id existence.
	otherCtx := tenantctx.With(t.Context(), "tenant-2")
	crossTenant := &models.PostAttachment{
		ID: "att-3", PostID: "post-x", MimeType: "image/png",
		SizeBytes: 1, ChecksumSHA256: "c", S3Key: "k3", CreatedBy: "user-1",
	}
	if err := repo.CreateAtNextPosition(otherCtx, crossTenant); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("attaching from another tenant to an existing post must fail closed with ErrNoRows, got %v", err)
	}
}

// TestReorderPositions verifies the transactional bulk renumber,
// including the case the frontend workaround produces: starting positions that
// have drifted into a non-contiguous high block rather than 0..n-1.
func TestReorderPositions(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewPostAttachmentRepository(db)
	ctx := tenantCtx()

	seedPost(t, db, "post-r", "", "", time.Now().UTC())

	ids := []string{"r0", "r1", "r2"}
	for _, id := range ids {
		att := &models.PostAttachment{
			ID: id, PostID: "post-r", MimeType: "image/png",
			SizeBytes: 1, ChecksumSHA256: "c-" + id, S3Key: "k-" + id, CreatedBy: "user-1",
		}
		if err := repo.CreateAtNextPosition(ctx, att); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	// Drift positions up into a non-contiguous block (5,6,7) to mimic the
	// frontend's max+1..max+n renumbering.
	for i, id := range ids {
		if err := repo.Patch(ctx, id, repository.AttachmentPatch{Position: new(5 + i)}); err != nil {
			t.Fatalf("drift %s: %v", id, err)
		}
	}

	// Reverse the order in one transactional call — must not trip the unique
	// constraint despite renumbering into positions siblings still hold.
	if err := repo.ReorderPositions(ctx, "post-r", []string{"r2", "r1", "r0"}); err != nil {
		t.Fatalf("reorder: %v", err)
	}

	got, err := repo.ListByPostID(ctx, "post-r")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"r2", "r1", "r0"} // ListByPostID orders by position
	if len(got) != len(want) {
		t.Fatalf("got %d attachments, want %d", len(got), len(want))
	}
	for i, a := range got {
		if a.ID != want[i] || a.Position != i {
			t.Errorf("pos %d: got id=%s position=%d, want id=%s position=%d", i, a.ID, a.Position, want[i], i)
		}
	}
}

// TestCoverKeysByPostIDs pins the cover pick: per post, the first drawable
// attachment — thread root before other segments, then by position — using a
// rendered thumbnail when there is one and the image itself otherwise. Media a
// browser cannot draw (HEIC, a video with no poster) is passed over, a post with
// nothing drawable is absent, and another tenant's posts are never returned.
func TestCoverKeysByPostIDs(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewPostAttachmentRepository(db)
	ctx := tenantCtx()

	add := func(id, postID, mime, key, thumb string) {
		t.Helper()
		att := &models.PostAttachment{
			ID: id, PostID: postID, MimeType: mime, SizeBytes: 1,
			ChecksumSHA256: id, S3Key: key, ThumbnailS3Key: thumb, CreatedBy: "user-1",
		}
		if err := repo.CreateAtNextPosition(ctx, att); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	for _, id := range []string{"p-img", "p-pdf", "p-skip", "p-none", "p-thread", "p-bare"} {
		seedPost(t, db, id, "", "", time.Now().UTC())
	}

	add("a1", "p-img", "image/png", "img-0.png", "")
	add("a2", "p-img", "image/jpeg", "img-1.jpg", "")

	add("b1", "p-pdf", "application/pdf", "deck.pdf", "deck.thumb.png")

	add("c1", "p-skip", "image/heic", "raw.heic", "")
	add("c2", "p-skip", "video/mp4", "clip.mp4", "")
	add("c3", "p-skip", "video/mp4", "clip2.mp4", "clip2.thumb.png")

	add("d1", "p-none", "image/heic", "only.heic", "")

	// Thread: position 0 belongs to segment 1, position 1 to the root.
	add("e1", "p-thread", "image/png", "reply.png", "")
	add("e2", "p-thread", "image/png", "root.png", "")
	if err := repo.Patch(ctx, "e1", repository.AttachmentPatch{SetSegmentIndex: true, SegmentIndex: new(1)}); err != nil {
		t.Fatalf("segment e1: %v", err)
	}
	if err := repo.Patch(ctx, "e2", repository.AttachmentPatch{SetSegmentIndex: true, SegmentIndex: new(0)}); err != nil {
		t.Fatalf("segment e2: %v", err)
	}

	got, err := repo.CoverKeysByPostIDs(ctx, []string{"p-img", "p-pdf", "p-skip", "p-none", "p-thread", "p-bare"})
	if err != nil {
		t.Fatalf("cover keys: %v", err)
	}
	want := map[string]string{
		"p-img":    "img-0.png",
		"p-pdf":    "deck.thumb.png",
		"p-skip":   "clip2.thumb.png",
		"p-thread": "root.png",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for id, key := range want {
		if got[id] != key {
			t.Errorf("%s: got %q, want %q", id, got[id], key)
		}
	}

	other, err := repo.CoverKeysByPostIDs(tenantctx.With(t.Context(), "tenant-2"), []string{"p-img"})
	if err != nil {
		t.Fatalf("cross-tenant cover keys: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("another tenant must see no covers, got %v", other)
	}

	empty, err := repo.CoverKeysByPostIDs(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("no ids: got %v, %v", empty, err)
	}
}
