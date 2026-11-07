package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// AssistantActionRepository stores the reversible changes the assistant made.
type AssistantActionRepository interface {
	Create(ctx context.Context, a *models.AssistantAction) error
	GetByID(ctx context.Context, id string) (*models.AssistantAction, error)
	// MarkUndone records the undo on db, a caller's transaction (nil uses the
	// repository's DB), so it commits with the restore. It reports false when
	// the action was already undone, so two undos of the same change count
	// once.
	MarkUndone(ctx context.Context, db bun.IDB, id, by string, now time.Time) (bool, error)
}

type assistantActionRepository struct {
	db *bun.DB
}

// NewAssistantActionRepository returns a Bun-backed AssistantActionRepository.
func NewAssistantActionRepository(db *bun.DB) AssistantActionRepository {
	return &assistantActionRepository{db: db}
}

// Create inserts a logged change.
func (r *assistantActionRepository) Create(ctx context.Context, a *models.AssistantAction) error {
	_, err := r.db.NewInsert().Model(a).Exec(ctx)
	return err
}

// GetByID returns the tenant's logged change, or sql.ErrNoRows.
func (r *assistantActionRepository) GetByID(ctx context.Context, id string) (*models.AssistantAction, error) {
	a := new(models.AssistantAction)
	if err := r.db.NewSelect().Model(a).Where("asa.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// MarkUndone stamps undone_at/undone_by on a change that isn't undone yet.
func (r *assistantActionRepository) MarkUndone(ctx context.Context, db bun.IDB, id, by string, now time.Time) (bool, error) {
	if db == nil {
		db = r.db
	}
	res, err := db.NewUpdate().Model((*models.AssistantAction)(nil)).
		Set("undone_at = ?", now).
		Set("undone_by = ?", by).
		Where("asa.id = ? AND asa.undone_at IS NULL", id).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
