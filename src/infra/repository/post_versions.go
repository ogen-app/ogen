package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PostVersionRepository handles persistence of post content version snapshots.
type PostVersionRepository interface {
	Create(ctx context.Context, version *models.PostVersion) error
	// CreateNext inserts version with the next free version_number for its
	// post, overwriting version.VersionNumber. The post row is locked for the
	// duration, so concurrent writers never collide on (post_id, version_number).
	CreateNext(ctx context.Context, version *models.PostVersion) error
	ListByPostID(ctx context.Context, postID string) ([]models.PostVersion, error)
	GetLatestByPostID(ctx context.Context, postID string) (*models.PostVersion, error)
	GetByPostIDAndVersionNumber(ctx context.Context, postID string, versionNumber int) (*models.PostVersion, error)
	CountByPostID(ctx context.Context, postID string) (int, error)
}

type postVersionRepository struct {
	db *bun.DB
}

func NewPostVersionRepository(db *bun.DB) PostVersionRepository {
	return &postVersionRepository{db: db}
}

func (r *postVersionRepository) Create(ctx context.Context, version *models.PostVersion) error {
	_, err := r.db.NewInsert().Model(version).Exec(ctx)
	return err
}

func (r *postVersionRepository) CreateNext(ctx context.Context, version *models.PostVersion) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var locked string
		if err := tx.NewSelect().
			Model((*models.Post)(nil)).
			Column("po.id").
			Where("po.id = ?", version.PostID).
			For("UPDATE").
			Scan(ctx, &locked); err != nil {
			return err
		}
		var latest int
		if err := tx.NewSelect().
			Model((*models.PostVersion)(nil)).
			ColumnExpr("COALESCE(MAX(pv.version_number), 0)").
			Where("pv.post_id = ?", version.PostID).
			Scan(ctx, &latest); err != nil {
			return err
		}
		version.VersionNumber = latest + 1
		_, err := tx.NewInsert().Model(version).Exec(ctx)
		return err
	})
}

func (r *postVersionRepository) ListByPostID(ctx context.Context, postID string) ([]models.PostVersion, error) {
	var versions []models.PostVersion
	err := r.db.NewSelect().
		Model(&versions).
		Where("pv.post_id = ?", postID).
		OrderExpr("pv.version_number ASC").
		Scan(ctx)
	return versions, err
}

func (r *postVersionRepository) GetLatestByPostID(ctx context.Context, postID string) (*models.PostVersion, error) {
	version := new(models.PostVersion)
	err := r.db.NewSelect().
		Model(version).
		Where("pv.post_id = ?", postID).
		OrderExpr("pv.version_number DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return version, nil
}

func (r *postVersionRepository) GetByPostIDAndVersionNumber(ctx context.Context, postID string, versionNumber int) (*models.PostVersion, error) {
	version := new(models.PostVersion)
	err := r.db.NewSelect().
		Model(version).
		Where("pv.post_id = ?", postID).
		Where("pv.version_number = ?", versionNumber).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return version, nil
}

func (r *postVersionRepository) CountByPostID(ctx context.Context, postID string) (int, error) {
	return r.db.NewSelect().
		Model((*models.PostVersion)(nil)).
		Where("post_id = ?", postID).
		Count(ctx)
}
