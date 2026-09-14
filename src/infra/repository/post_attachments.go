package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PostAttachmentRepository persists image attachments bound to a Post (CON-73).
type PostAttachmentRepository interface {
	ListByPostID(ctx context.Context, postID string) ([]models.PostAttachment, error)
	ListS3KeysByPostID(ctx context.Context, postID string) ([]string, error)
	GetByID(ctx context.Context, id string) (*models.PostAttachment, error)
	// CreateAtNextPosition inserts att and assigns it the next free
	// position for its post in a single atomic statement, so concurrent
	// uploads to the same post never trip the (post_id, position)
	// unique constraint. att.Position is overwritten with the assigned
	// value on success.
	CreateAtNextPosition(ctx context.Context, att *models.PostAttachment) error
	UpdatePosition(ctx context.Context, id string, position int) error
	UpdateAltText(ctx context.Context, id string, altText string) error
	// UpdateSegmentIndex reassigns which thread segment an attachment belongs
	// to (CON-284). A nil segmentIndex clears it (detach from any segment,
	// i.e. back to a non-thread attachment).
	UpdateSegmentIndex(ctx context.Context, id string, segmentIndex *int) error
	// ReorderPositions renumbers the post's attachments to 0..n-1 to match
	// orderedIDs, in one transaction, without tripping UNIQUE(post_id, position)
	// (CON-124). Callers must pass every current attachment of the post exactly
	// once.
	ReorderPositions(ctx context.Context, postID string, orderedIDs []string) error
	Delete(ctx context.Context, id string) (bool, error)
	// SumSizeBytesInTenant totals size_bytes across every attachment in the ctx
	// tenant — the live usage behind the media_storage_bytes quota (CON-295).
	SumSizeBytesInTenant(ctx context.Context) (int64, error)
}

type postAttachmentRepository struct {
	db *bun.DB
}

func NewPostAttachmentRepository(db *bun.DB) PostAttachmentRepository {
	return &postAttachmentRepository{db: db}
}

func (r *postAttachmentRepository) ListByPostID(ctx context.Context, postID string) ([]models.PostAttachment, error) {
	var atts []models.PostAttachment
	err := r.db.NewSelect().
		Model(&atts).
		Where("pa.post_id = ?", postID).
		OrderExpr("position ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return atts, nil
}

func (r *postAttachmentRepository) SumSizeBytesInTenant(ctx context.Context) (int64, error) {
	var total int64
	err := r.db.NewSelect().
		Model((*models.PostAttachment)(nil)).
		ColumnExpr("COALESCE(SUM(pa.size_bytes), 0)").
		Scan(ctx, &total)
	return total, err
}

func (r *postAttachmentRepository) GetByID(ctx context.Context, id string) (*models.PostAttachment, error) {
	att := new(models.PostAttachment)
	err := r.db.NewSelect().Model(att).Where("pa.id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return att, nil
}

func (r *postAttachmentRepository) ListS3KeysByPostID(ctx context.Context, postID string) ([]string, error) {
	// Returns both the primary blob key and any non-empty thumbnail key
	// (CON-75) so the cascade-delete hook clears every object owned by
	// the post in one pass.
	var rows []struct {
		S3Key          string `bun:"s3_key"`
		ThumbnailS3Key string `bun:"thumbnail_s3_key"`
	}
	err := r.db.NewSelect().
		Model((*models.PostAttachment)(nil)).
		Column("s3_key", "thumbnail_s3_key").
		Where("post_id = ?", postID).
		Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(rows)*2)
	for _, row := range rows {
		if row.S3Key != "" {
			keys = append(keys, row.S3Key)
		}
		if row.ThumbnailS3Key != "" {
			keys = append(keys, row.ThumbnailS3Key)
		}
	}
	return keys, nil
}

func (r *postAttachmentRepository) CreateAtNextPosition(ctx context.Context, att *models.PostAttachment) error {
	// This INSERT is raw SQL, so the TenantScoped hooks don't fire — stamp and
	// scope tenant_id by hand (CON-97).
	tid, err := writeTenantID(ctx, att.TenantID)
	if err != nil {
		return err
	}
	att.TenantID = tid

	var thumb any
	if att.ThumbnailS3Key != "" {
		thumb = att.ThumbnailS3Key
	}
	// Lock the parent post row (within the tenant) for the duration of the
	// insert so two concurrent uploads to the same post serialise — otherwise
	// both could read the same MAX(position) and collide on the (post_id,
	// position) unique constraint. Postgres runs writers concurrently (SQLite
	// did not), so the serialisation must be explicit.
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Scan (not Exec) so a zero-row lock is detected: no parent post in this
		// tenant (deleted mid-upload, or a cross-tenant id) fails closed with
		// sql.ErrNoRows instead of inserting an orphaned/cross-tenant attachment.
		var locked int
		if err := tx.NewRaw(`SELECT 1 FROM posts WHERE id = ? AND tenant_id = ? FOR UPDATE`, att.PostID, tid).Scan(ctx, &locked); err != nil {
			return err
		}
		// segment_index (CON-284) is NULL for non-thread attachments; a nil
		// *int binds as NULL, a non-nil pointer as the 0-based segment.
		const q = `INSERT INTO post_attachments
			(id, post_id, tenant_id, position, segment_index, mime_type, size_bytes, width, height,
			 is_animated, page_count, checksum_sha256, s3_key, thumbnail_s3_key, created_by)
			VALUES (?, ?, ?, COALESCE((SELECT MAX(position)+1 FROM post_attachments WHERE post_id=? AND tenant_id=?), 0),
			        ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING position`
		return tx.NewRaw(q,
			att.ID, att.PostID, tid, att.PostID, tid,
			att.SegmentIndex,
			att.MimeType, att.SizeBytes, att.Width, att.Height,
			att.IsAnimated, att.PageCount, att.ChecksumSHA256, att.S3Key, thumb, att.CreatedBy,
		).Scan(ctx, &att.Position)
	})
}

func (r *postAttachmentRepository) UpdatePosition(ctx context.Context, id string, position int) error {
	_, err := r.db.NewUpdate().
		Model((*models.PostAttachment)(nil)).
		Set("position = ?", position).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

func (r *postAttachmentRepository) UpdateAltText(ctx context.Context, id string, altText string) error {
	_, err := r.db.NewUpdate().
		Model((*models.PostAttachment)(nil)).
		Set("alt_text = ?", altText).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

func (r *postAttachmentRepository) UpdateSegmentIndex(ctx context.Context, id string, segmentIndex *int) error {
	// bun binds a nil *int as NULL, a non-nil pointer as the value — so this
	// both sets and clears segment_index (CON-284).
	_, err := r.db.NewUpdate().
		Model((*models.PostAttachment)(nil)).
		Set("segment_index = ?", segmentIndex).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

func (r *postAttachmentRepository) ReorderPositions(ctx context.Context, postID string, orderedIDs []string) error {
	// Model queries below fire the TenantScoped hooks, so every statement is
	// auto-scoped to the caller's tenant (this runs in the request context).
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Shift every attachment of the post into a disjoint high range first.
		// UNIQUE(post_id, position) is checked per row, so renumbering to 0..n-1
		// directly would transiently collide with a sibling that still holds a
		// target position. Choosing offset = maxPos+1 keeps the shifted range
		// (> maxPos) disjoint from the target range (0..n-1), so the shift and
		// the renumber are both collision-free row by row.
		var maxPos int
		if err := tx.NewSelect().
			Model((*models.PostAttachment)(nil)).
			ColumnExpr("COALESCE(MAX(position), -1)").
			Where("post_id = ?", postID).
			Scan(ctx, &maxPos); err != nil {
			return err
		}
		if _, err := tx.NewUpdate().
			Model((*models.PostAttachment)(nil)).
			Set("position = position + ?", maxPos+1).
			Where("post_id = ?", postID).
			Exec(ctx); err != nil {
			return err
		}
		for i, id := range orderedIDs {
			if _, err := tx.NewUpdate().
				Model((*models.PostAttachment)(nil)).
				Set("position = ?", i).
				Where("id = ?", id).
				Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *postAttachmentRepository) Delete(ctx context.Context, id string) (bool, error) {
	res, err := r.db.NewDelete().
		Model((*models.PostAttachment)(nil)).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
