package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

var (
	// ErrPendingUploadGone: the upload's pending row no longer exists, so the
	// sweep has already deleted (or is deleting) its object.
	ErrPendingUploadGone = errors.New("pending upload expired")
	// ErrUploadAlreadyAttached: an earlier finalize of the same key won, and
	// its attachment already holds the object.
	ErrUploadAlreadyAttached = errors.New("upload already attached")
)

// ReapOutcome is what Reap did with one pending upload.
type ReapOutcome int

const (
	// ReapSkipped: the row is gone, not yet expired or locked by a finalize in
	// flight. Nothing changed.
	ReapSkipped ReapOutcome = iota
	// ReapAttached: an attachment holds the key, so only the row was dropped.
	ReapAttached
	// ReapRemoved: the object was removed and the row dropped.
	ReapRemoved
)

// PendingUploadRepository records presigned uploads until finalize consumes
// them (PostAttachmentRepository.CreateFromPendingUpload), so the ones never
// finalized can be swept.
type PendingUploadRepository interface {
	Create(ctx context.Context, u *models.PendingUpload) error
	// Discard disposes of an upload finalize refused, in the ctx tenant. It
	// locks the key's record, waiting out a concurrent finalize, and leaves
	// the object alone if that finalize attached it. Otherwise remove deletes
	// the object and the record goes with it; a remove error rolls back and
	// keeps the record for the sweep. A key with no record is still removed:
	// no finalize can attach it any more.
	Discard(ctx context.Context, key string, remove func(context.Context) error) error
	// ListExpired returns up to limit uploads that expired before cutoff,
	// oldest first. Under a system context it spans every tenant.
	ListExpired(ctx context.Context, cutoff time.Time, limit int) ([]models.PendingUpload, error)
	// Reap settles one expired upload under its row lock: an attached key only
	// loses its row, otherwise remove deletes the object and the row goes with
	// it. A remove error rolls back and keeps the row for the next sweep. A row
	// locked by a concurrent finalize is skipped, not waited on.
	Reap(ctx context.Context, id string, cutoff time.Time, remove func(context.Context, models.PendingUpload) error) (ReapOutcome, error)
}

type pendingUploadRepository struct {
	db *bun.DB
}

func NewPendingUploadRepository(db *bun.DB) PendingUploadRepository {
	return &pendingUploadRepository{db: db}
}

func (r *pendingUploadRepository) Create(ctx context.Context, u *models.PendingUpload) error {
	_, err := r.db.NewInsert().Model(u).Exec(ctx)
	return err
}

func (r *pendingUploadRepository) Discard(ctx context.Context, key string, remove func(context.Context) error) error {
	tid, err := writeTenantID(ctx, "")
	if err != nil {
		return err
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var locked []string
		if err := tx.NewRaw(`SELECT id FROM pending_uploads WHERE s3_key = ? AND tenant_id = ? FOR UPDATE`, key, tid).
			Scan(ctx, &locked); err != nil {
			return err
		}
		attached, err := keyAttached(ctx, tx, key, tid)
		if err != nil || attached {
			return err
		}
		if err := remove(ctx); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM pending_uploads WHERE s3_key = ? AND tenant_id = ?`, key, tid)
		return err
	})
}

func (r *pendingUploadRepository) ListExpired(ctx context.Context, cutoff time.Time, limit int) ([]models.PendingUpload, error) {
	if limit <= 0 {
		limit = 500
	}
	var rows []models.PendingUpload
	err := r.db.NewSelect().
		Model(&rows).
		Where("pu.expires_at < ?", cutoff).
		OrderExpr("pu.expires_at ASC").
		Limit(limit).
		Scan(ctx)
	return rows, err
}

func (r *pendingUploadRepository) Reap(ctx context.Context, id string, cutoff time.Time, remove func(context.Context, models.PendingUpload) error) (ReapOutcome, error) {
	outcome := ReapSkipped
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var u models.PendingUpload
		err := tx.NewSelect().
			Model(&u).
			Where("pu.id = ?", id).
			Where("pu.expires_at < ?", cutoff).
			For("UPDATE SKIP LOCKED").
			Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		attached, err := keyAttached(ctx, tx, u.S3Key, u.TenantID)
		if err != nil {
			return err
		}
		if !attached {
			if err := remove(ctx, u); err != nil {
				return err
			}
		}
		if _, err := tx.NewDelete().Model((*models.PendingUpload)(nil)).Where("id = ?", u.ID).Exec(ctx); err != nil {
			return err
		}
		outcome = ReapRemoved
		if attached {
			outcome = ReapAttached
		}
		return nil
	})
	if err != nil {
		return ReapSkipped, err
	}
	return outcome, nil
}

// keyAttached reports whether a post attachment in the tenant holds key.
func keyAttached(ctx context.Context, db bun.IDB, key, tenantID string) (bool, error) {
	var attached bool
	err := db.NewRaw(`SELECT EXISTS (SELECT 1 FROM post_attachments WHERE s3_key = ? AND tenant_id = ?)`, key, tenantID).
		Scan(ctx, &attached)
	return attached, err
}

// consumePendingUpload deletes key's pending row inside tx, waiting out a
// sweep that holds its lock. With no row left it tells a repeated finalize
// (the key is already attached) from one the sweep beat.
func consumePendingUpload(ctx context.Context, tx bun.Tx, key, tenantID string) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM pending_uploads WHERE s3_key = ? AND tenant_id = ?`, key, tenantID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	attached, err := keyAttached(ctx, tx, key, tenantID)
	if err != nil {
		return err
	}
	if attached {
		return ErrUploadAlreadyAttached
	}
	return ErrPendingUploadGone
}
