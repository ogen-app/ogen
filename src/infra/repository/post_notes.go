package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PostNoteRepository persists per-post Notes. Every query runs through
// bun's Model API so the TenantScoped hooks auto-stamp/scope tenant_id — no
// method has to remember the tenant predicate.
type PostNoteRepository interface {
	Create(ctx context.Context, note *models.PostNote) error
	ListByPostID(ctx context.Context, postID string) ([]models.PostNote, error)
	GetByID(ctx context.Context, id string) (*models.PostNote, error)
	// Update writes title/body/type and bumps updated_at. Returns false when
	// no row matched (unknown id or cross-tenant).
	Update(ctx context.Context, note *models.PostNote) (bool, error)
	Delete(ctx context.Context, id string) (bool, error)
}

type postNoteRepository struct {
	db *bun.DB
}

func NewPostNoteRepository(db *bun.DB) PostNoteRepository {
	return &postNoteRepository{db: db}
}

func (r *postNoteRepository) Create(ctx context.Context, note *models.PostNote) error {
	_, err := r.db.NewInsert().Model(note).Exec(ctx)
	return err
}

// ListByPostID returns a post's notes with draft_thesis notes pinned to the top,
// everything else following in oldest-first order.
func (r *postNoteRepository) ListByPostID(ctx context.Context, postID string) ([]models.PostNote, error) {
	var notes []models.PostNote
	err := r.db.NewSelect().
		Model(&notes).
		Where("pn.post_id = ?", postID).
		OrderExpr("(pn.type = ?) DESC, pn.created_at ASC", string(models.PostNoteTypeDraftThesis)).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return notes, nil
}

func (r *postNoteRepository) GetByID(ctx context.Context, id string) (*models.PostNote, error) {
	note := new(models.PostNote)
	err := r.db.NewSelect().Model(note).Where("pn.id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return note, nil
}

func (r *postNoteRepository) Update(ctx context.Context, note *models.PostNote) (bool, error) {
	note.UpdatedAt = time.Now().UTC()
	res, err := r.db.NewUpdate().
		Model(note).
		Column("title", "body", "type", "updated_at").
		WherePK().
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *postNoteRepository) Delete(ctx context.Context, id string) (bool, error) {
	res, err := r.db.NewDelete().
		Model((*models.PostNote)(nil)).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
