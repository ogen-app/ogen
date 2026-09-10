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
	// fails closed (CON-97) rather than inserting a cross-tenant attachment.
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

// TestReorderPositions verifies the transactional bulk renumber (CON-124),
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
		if err := repo.UpdatePosition(ctx, id, 5+i); err != nil {
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
