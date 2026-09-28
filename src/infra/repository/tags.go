package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// TagRepository defines all persistence operations for the Tag domain.
type TagRepository interface {
	List(ctx context.Context) ([]models.Tag, error)
	Create(ctx context.Context, tag *models.Tag) error
	GetByID(ctx context.Context, id string) (*models.Tag, error)
	// GetByIDs fetches the tags for the given IDs and returns them indexed by ID.
	// Unknown IDs are silently ignored. An empty slice returns an empty map.
	GetByIDs(ctx context.Context, ids []string) (map[string]models.Tag, error)
	Update(ctx context.Context, tag *models.Tag) error
	Delete(ctx context.Context, id string) (bool, error)
}

type tagRepository struct {
	db *bun.DB
}

// NewTagRepository returns a Bun-backed TagRepository.
func NewTagRepository(db *bun.DB) TagRepository {
	return &tagRepository{db: db}
}

func (r *tagRepository) List(ctx context.Context) ([]models.Tag, error) {
	var tags []models.Tag
	if err := r.db.NewSelect().Model(&tags).OrderExpr("created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	return tags, nil
}

func (r *tagRepository) Create(ctx context.Context, tag *models.Tag) error {
	_, err := r.db.NewInsert().Model(tag).Exec(ctx)
	return err
}

func (r *tagRepository) GetByIDs(ctx context.Context, ids []string) (map[string]models.Tag, error) {
	if len(ids) == 0 {
		return map[string]models.Tag{}, nil
	}
	var tags []models.Tag
	if err := r.db.NewSelect().Model(&tags).Where("id IN (?)", bun.List(ids)).Scan(ctx); err != nil {
		return nil, err
	}
	index := make(map[string]models.Tag, len(tags))
	for _, t := range tags {
		index[t.ID] = t
	}
	return index, nil
}

func (r *tagRepository) GetByID(ctx context.Context, id string) (*models.Tag, error) {
	tag := new(models.Tag)
	err := r.db.NewSelect().Model(tag).Where("t.id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return tag, nil
}

func (r *tagRepository) Update(ctx context.Context, tag *models.Tag) error {
	_, err := r.db.NewUpdate().Model(tag).WherePK().Exec(ctx)
	return err
}

func (r *tagRepository) Delete(ctx context.Context, id string) (bool, error) {
	res, err := r.db.NewDelete().Model((*models.Tag)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
