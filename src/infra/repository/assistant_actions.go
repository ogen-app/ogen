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
	// MarkUndone records the undo. It reports false when the action was
	// already undone, so two undos of the same change count once.
	MarkUndone(ctx context.Context, id, by string, now time.Time) (bool, error)
}

type assistantActionRepository struct {
	db *bun.DB
}

func NewAssistantActionRepository(db *bun.DB) AssistantActionRepository {
	return &assistantActionRepository{db: db}
}

func (r *assistantActionRepository) Create(ctx context.Context, a *models.AssistantAction) error {
	_, err := r.db.NewInsert().Model(a).Exec(ctx)
	return err
}

func (r *assistantActionRepository) GetByID(ctx context.Context, id string) (*models.AssistantAction, error) {
	a := new(models.AssistantAction)
	if err := r.db.NewSelect().Model(a).Where("asa.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func (r *assistantActionRepository) MarkUndone(ctx context.Context, id, by string, now time.Time) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.AssistantAction)(nil)).
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
