package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// SeriesRepository persists series and the campaign runs of them. Every query
// runs through bun's Model API so the TenantScoped hooks scope tenant_id, and
// every read skips soft-deleted series.
type SeriesRepository interface {
	// List returns every live series in the workspace, both scopes, oldest
	// first.
	List(ctx context.Context) ([]models.Series, error)
	// GetByID returns sql.ErrNoRows for an unknown, cross-tenant or deleted id.
	GetByID(ctx context.Context, id string) (*models.Series, error)
	// Create inserts the series. A campaign-local series is attached to its
	// campaign at its default rhythm in the same transaction.
	Create(ctx context.Context, s *models.Series) error
	// Update writes the named columns plus updated_at. Returns false when no
	// live row matched.
	Update(ctx context.Context, s *models.Series, columns ...string) (bool, error)
	// SoftDelete stamps deleted_at and removes the series' campaign runs in
	// one transaction. Returns false when no live row matched.
	SoftDelete(ctx context.Context, id string) (bool, error)
	// Usage counts posts per series id. Ids with no posts are absent.
	Usage(ctx context.Context, ids []string) (map[string]models.SeriesUsage, error)

	// Runs returns the campaign's runs, oldest attach first. Never nil.
	Runs(ctx context.Context, campaignID string) ([]models.CampaignSeriesRun, error)
	// Attach inserts a run, leaving an existing one (and its rhythm) as it is.
	Attach(ctx context.Context, run *models.CampaignSeriesRun) error
	Detach(ctx context.Context, campaignID, seriesID string) error
	// SetRhythm returns false when the campaign does not run the series.
	SetRhythm(ctx context.Context, campaignID, seriesID string, rhythm *models.SeriesRhythm) (bool, error)

	// LiveCampaignExists reports whether campaignID names a campaign in the
	// caller's tenant that is not soft-deleted (archived counts as live).
	LiveCampaignExists(ctx context.Context, campaignID string) (bool, error)
}

type seriesRepository struct {
	db *bun.DB
}

func NewSeriesRepository(db *bun.DB) SeriesRepository {
	return &seriesRepository{db: db}
}

func (r *seriesRepository) List(ctx context.Context) ([]models.Series, error) {
	list := []models.Series{}
	err := r.db.NewSelect().
		Model(&list).
		Where("bs.deleted_at IS NULL").
		OrderExpr("bs.created_at ASC, bs.id ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return list, nil
}

func (r *seriesRepository) GetByID(ctx context.Context, id string) (*models.Series, error) {
	s := new(models.Series)
	err := r.db.NewSelect().
		Model(s).
		Where("bs.id = ?", id).
		Where("bs.deleted_at IS NULL").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (r *seriesRepository) Create(ctx context.Context, s *models.Series) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(s).Exec(ctx); err != nil {
			return err
		}
		if s.CampaignID == nil {
			return nil
		}
		run := &models.CampaignSeriesRun{
			CampaignID: *s.CampaignID,
			SeriesID:   s.ID,
			Rhythm:     s.DefaultRhythm,
			CreatedAt:  s.CreatedAt,
		}
		_, err := tx.NewInsert().Model(run).Exec(ctx)
		return err
	})
}

func (r *seriesRepository) Update(ctx context.Context, s *models.Series, columns ...string) (bool, error) {
	res, err := r.db.NewUpdate().
		Model(s).
		Column(append(columns, "updated_at")...).
		WherePK().
		Where("deleted_at IS NULL").
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *seriesRepository) SoftDelete(ctx context.Context, id string) (bool, error) {
	var deleted bool
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().
			Model((*models.Series)(nil)).
			Set("deleted_at = ?", time.Now().UTC()).
			Where("id = ?", id).
			Where("deleted_at IS NULL").
			Exec(ctx)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if deleted = n > 0; !deleted {
			return nil
		}
		_, err = tx.NewDelete().
			Model((*models.CampaignSeriesRun)(nil)).
			Where("series_id = ?", id).
			Exec(ctx)
		return err
	})
	return deleted, err
}

func (r *seriesRepository) Usage(ctx context.Context, ids []string) (map[string]models.SeriesUsage, error) {
	out := make(map[string]models.SeriesUsage, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []struct {
		SeriesID  string `bun:"series_id"`
		Drafts    int    `bun:"drafts"`
		Published int    `bun:"published"`
	}
	err := r.db.NewSelect().
		Model((*models.Post)(nil)).
		ColumnExpr("po.series_id").
		ColumnExpr("count(*) FILTER (WHERE po.status <> ?) AS drafts", models.PostStatusPublished).
		ColumnExpr("count(*) FILTER (WHERE po.status = ?) AS published", models.PostStatusPublished).
		Where("po.series_id IN (?)", bun.List(ids)).
		GroupExpr("po.series_id").
		Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.SeriesID] = models.SeriesUsage{Drafts: row.Drafts, Published: row.Published}
	}
	return out, nil
}

func (r *seriesRepository) Runs(ctx context.Context, campaignID string) ([]models.CampaignSeriesRun, error) {
	runs := []models.CampaignSeriesRun{}
	err := r.db.NewSelect().
		Model(&runs).
		Where("cs.campaign_id = ?", campaignID).
		OrderExpr("cs.created_at ASC, cs.series_id ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return runs, nil
}

func (r *seriesRepository) Attach(ctx context.Context, run *models.CampaignSeriesRun) error {
	_, err := r.db.NewInsert().
		Model(run).
		On("CONFLICT (campaign_id, series_id) DO NOTHING").
		Exec(ctx)
	return err
}

func (r *seriesRepository) Detach(ctx context.Context, campaignID, seriesID string) error {
	_, err := r.db.NewDelete().
		Model((*models.CampaignSeriesRun)(nil)).
		Where("campaign_id = ?", campaignID).
		Where("series_id = ?", seriesID).
		Exec(ctx)
	return err
}

func (r *seriesRepository) SetRhythm(ctx context.Context, campaignID, seriesID string, rhythm *models.SeriesRhythm) (bool, error) {
	res, err := r.db.NewUpdate().
		Model(&models.CampaignSeriesRun{Rhythm: rhythm}).
		Column("rhythm").
		Where("campaign_id = ?", campaignID).
		Where("series_id = ?", seriesID).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// LiveCampaignExists uses Scan, not Exists: bun's Exists skips the
// BeforeSelect hook, so a campaign from another tenant would pass.
func (r *seriesRepository) LiveCampaignExists(ctx context.Context, campaignID string) (bool, error) {
	var id string
	err := r.db.NewSelect().
		Model((*models.Campaign)(nil)).
		Column("c.id").
		Where("c.id = ?", campaignID).
		Where("c.deleted_at IS NULL").
		Limit(1).
		Scan(ctx, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
