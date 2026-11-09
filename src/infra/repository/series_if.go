package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// ErrSeriesChanged is a conditional series or run write that found it
// changed since it was read.
var ErrSeriesChanged = errors.New("series changed since it was read")

// SeriesEditable are the columns an edit of a series writes.
var SeriesEditable = []string{"name", "promise", "recipe", "supply", "content_format", "default_rhythm"}

// SameSeries reports whether two series hold the same editable fields and
// scope.
func SameSeries(a, b *models.Series) bool {
	return a.Name == b.Name && a.Promise == b.Promise && a.Recipe == b.Recipe && a.Supply == b.Supply &&
		samePtr(a.ContentFormat, b.ContentFormat) && samePtr(a.DefaultRhythm, b.DefaultRhythm) && samePtr(a.CampaignID, b.CampaignID)
}

// SameRun reports whether two runs are the same; nil is a series the
// campaign doesn't run.
func SameRun(a, b *models.CampaignSeriesRun) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return samePtr(a.Rhythm, b.Rhythm)
}

func samePtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (r *seriesRepository) inTx(ctx context.Context, db bun.IDB, fn func(ctx context.Context, tx bun.IDB) error) error {
	if db != nil {
		return fn(ctx, db)
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error { return fn(ctx, tx) })
}

// lockSeries reads series id, deleted or not, and holds its row.
func lockSeries(ctx context.Context, tx bun.IDB, id string) (*models.Series, error) {
	s := new(models.Series)
	if err := tx.NewSelect().Model(s).Where("bs.id = ?", id).For("UPDATE").Scan(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// UpdateIf writes s's editable columns on db (nil runs its own transaction)
// only while the live series still equals was; ErrSeriesChanged otherwise,
// ErrSeriesNotLive when it is gone.
func (r *seriesRepository) UpdateIf(ctx context.Context, db bun.IDB, s, was *models.Series) error {
	return r.inTx(ctx, db, func(ctx context.Context, tx bun.IDB) error {
		cur, err := lockSeries(ctx, tx, s.ID)
		if errors.Is(err, sql.ErrNoRows) || err == nil && cur.DeletedAt != nil {
			return ErrSeriesNotLive
		}
		if err != nil {
			return err
		}
		if !SameSeries(cur, was) {
			return ErrSeriesChanged
		}
		_, err = tx.NewUpdate().Model(s).Column(append(SeriesEditable, "updated_at")...).WherePK().Exec(ctx)
		return err
	})
}

// SoftDeleteIf soft-deletes a live series on db (nil runs its own
// transaction) while it equals was (nil: whatever it holds), and returns the
// runs it removed. ErrSeriesNotLive when it is gone.
func (r *seriesRepository) SoftDeleteIf(ctx context.Context, db bun.IDB, id string, was *models.Series) ([]models.CampaignSeriesRun, error) {
	var runs []models.CampaignSeriesRun
	err := r.inTx(ctx, db, func(ctx context.Context, tx bun.IDB) error {
		cur, err := lockSeries(ctx, tx, id)
		if errors.Is(err, sql.ErrNoRows) || err == nil && cur.DeletedAt != nil {
			return ErrSeriesNotLive
		}
		if err != nil {
			return err
		}
		if was != nil && !SameSeries(cur, was) {
			return ErrSeriesChanged
		}
		runs = []models.CampaignSeriesRun{}
		if err := tx.NewSelect().Model(&runs).Where("cs.series_id = ?", id).OrderExpr("cs.created_at, cs.campaign_id").
			Scan(ctx); err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model((*models.Series)(nil)).Set("deleted_at = ?", time.Now().UTC()).
			Where("id = ?", id).Exec(ctx); err != nil {
			return err
		}
		_, err = tx.NewDelete().Model((*models.CampaignSeriesRun)(nil)).Where("series_id = ?", id).Exec(ctx)
		return err
	})
	return runs, err
}

// Undelete brings a soft-deleted series back on db (nil runs its own
// transaction) while it still equals was, with the runs it had on campaigns
// still live. ErrSeriesChanged when it is live again or was edited.
func (r *seriesRepository) Undelete(ctx context.Context, db bun.IDB, was *models.Series, runs []models.CampaignSeriesRun) error {
	return r.inTx(ctx, db, func(ctx context.Context, tx bun.IDB) error {
		cur, err := lockSeries(ctx, tx, was.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSeriesChanged
		}
		if err != nil {
			return err
		}
		if cur.DeletedAt == nil || !SameSeries(cur, was) {
			return ErrSeriesChanged
		}
		if _, err := tx.NewUpdate().Model((*models.Series)(nil)).Set("deleted_at = NULL").
			Where("id = ?", was.ID).Exec(ctx); err != nil {
			return err
		}
		for _, run := range runs {
			ok, err := liveCampaign(ctx, tx, run.CampaignID)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			run.SeriesID = was.ID
			if _, err := tx.NewInsert().Model(&run).On("CONFLICT (campaign_id, series_id) DO NOTHING").Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}

// Run returns the campaign's run of a series; nil when it doesn't run it.
func (r *seriesRepository) Run(ctx context.Context, campaignID, seriesID string) (*models.CampaignSeriesRun, error) {
	run := new(models.CampaignSeriesRun)
	err := r.db.NewSelect().Model(run).Where("cs.campaign_id = ?", campaignID).Where("cs.series_id = ?", seriesID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return run, err
}

// SetRunIf sets the campaign's run of a series to want (nil: not run) on db
// (nil runs its own transaction), only while it still equals was;
// ErrSeriesChanged otherwise. Running it needs the campaign and the series
// live: ErrCampaignNotLive, ErrSeriesNotLive.
func (r *seriesRepository) SetRunIf(ctx context.Context, db bun.IDB, campaignID, seriesID string, want, was *models.CampaignSeriesRun) error {
	return r.inTx(ctx, db, func(ctx context.Context, tx bun.IDB) error {
		if err := tenantLock(ctx, tx, "campaign_series:"+campaignID); err != nil {
			return err
		}
		cur := new(models.CampaignSeriesRun)
		err := tx.NewSelect().Model(cur).Where("cs.campaign_id = ?", campaignID).Where("cs.series_id = ?", seriesID).Scan(ctx)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			cur = nil
		case err != nil:
			return err
		}
		if !SameRun(cur, was) {
			return ErrSeriesChanged
		}
		if want == nil {
			if cur == nil {
				return nil
			}
			_, err := tx.NewDelete().Model((*models.CampaignSeriesRun)(nil)).
				Where("campaign_id = ?", campaignID).Where("series_id = ?", seriesID).Exec(ctx)
			return err
		}
		if ok, err := liveCampaign(ctx, tx, campaignID); err != nil || !ok {
			return cmpErr(err, ErrCampaignNotLive)
		}
		s, err := lockSeries(ctx, tx, seriesID)
		if errors.Is(err, sql.ErrNoRows) || err == nil && s.DeletedAt != nil {
			return ErrSeriesNotLive
		}
		if err != nil {
			return err
		}
		if cur != nil {
			_, err := tx.NewUpdate().Model(&models.CampaignSeriesRun{Rhythm: want.Rhythm}).Column("rhythm").
				Where("campaign_id = ?", campaignID).Where("series_id = ?", seriesID).Exec(ctx)
			return err
		}
		run := &models.CampaignSeriesRun{CampaignID: campaignID, SeriesID: seriesID, Rhythm: want.Rhythm, CreatedAt: time.Now().UTC()}
		_, err = tx.NewInsert().Model(run).Exec(ctx)
		return err
	})
}

func cmpErr(err, otherwise error) error {
	if err != nil {
		return err
	}
	return otherwise
}

func liveCampaign(ctx context.Context, db bun.IDB, campaignID string) (bool, error) {
	var id string
	err := db.NewSelect().Model((*models.Campaign)(nil)).Column("c.id").Where("c.id = ?", campaignID).
		Where("c.deleted_at IS NULL").Limit(1).Scan(ctx, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
