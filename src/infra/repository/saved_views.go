package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// SavedViewRepository persists saved views. Every query runs through bun's
// Model API so the TenantScoped hooks scope tenant_id.
type SavedViewRepository interface {
	// ListVisible returns the views of surface that userID sees, its own and
	// every shared one, newest first.
	ListVisible(ctx context.Context, surface, userID string) ([]models.SavedView, error)
	// GetByID returns sql.ErrNoRows for an unknown or cross-tenant id.
	GetByID(ctx context.Context, id string) (*models.SavedView, error)
	Create(ctx context.Context, view *models.SavedView) error
	// Update writes only the named columns plus updated_at. Returns false when
	// no row matched.
	Update(ctx context.Context, view *models.SavedView, columns ...string) (bool, error)
	Delete(ctx context.Context, id string) (bool, error)
}

type savedViewRepository struct {
	db *bun.DB
}

func NewSavedViewRepository(db *bun.DB) SavedViewRepository {
	return &savedViewRepository{db: db}
}

func (r *savedViewRepository) ListVisible(ctx context.Context, surface, userID string) ([]models.SavedView, error) {
	views := []models.SavedView{}
	err := r.db.NewSelect().Model(&views).
		Where("sv.surface = ?", surface).
		Where("(sv.shared OR sv.created_by = ?)", userID).
		OrderExpr("sv.created_at DESC, sv.id DESC").
		Scan(ctx)
	return views, err
}

func (r *savedViewRepository) GetByID(ctx context.Context, id string) (*models.SavedView, error) {
	view := new(models.SavedView)
	if err := r.db.NewSelect().Model(view).Where("sv.id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return view, nil
}

func (r *savedViewRepository) Create(ctx context.Context, view *models.SavedView) error {
	_, err := r.db.NewInsert().Model(view).Exec(ctx)
	return err
}

func (r *savedViewRepository) Update(ctx context.Context, view *models.SavedView, columns ...string) (bool, error) {
	res, err := r.db.NewUpdate().Model(view).Column(append(columns, "updated_at")...).WherePK().Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *savedViewRepository) Delete(ctx context.Context, id string) (bool, error) {
	res, err := r.db.NewDelete().Model((*models.SavedView)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// deletePrivateViewsOf deletes the private saved views of a member about to
// be removed; the FK then clears created_by on their shared ones.
func deletePrivateViewsOf(ctx context.Context, db bun.IDB, userID string) error {
	_, err := db.NewDelete().TableExpr("saved_views").
		Where("created_by = ?", userID).
		Where("NOT shared").
		Exec(ctx)
	return err
}
