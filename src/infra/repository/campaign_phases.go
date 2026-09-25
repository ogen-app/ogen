package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// CON-166: campaign phase plan persistence + the phase-integrity helpers. These
// hang off campaignRepository (the plan is campaign-owned state); every query
// goes through bun's Model API so the TenantScoped hooks scope it.

// hydrateTypeLocked sets PhasedPostCount / TypeLocked with one grouped query
// over the batch: a campaign's type is locked once any of its posts is planned
// against a phase.
func (r *campaignRepository) hydrateTypeLocked(ctx context.Context, campaigns []models.Campaign) error {
	if len(campaigns) == 0 {
		return nil
	}
	ids := make([]string, len(campaigns))
	for i, c := range campaigns {
		ids[i] = c.ID
	}
	var rows []struct {
		CampaignID string `bun:"campaign_id"`
		N          int    `bun:"n"`
	}
	err := r.db.NewSelect().
		Model((*models.Post)(nil)).
		ColumnExpr("po.campaign_id AS campaign_id").
		ColumnExpr("count(*) AS n").
		Where("po.campaign_id IN (?)", bun.In(ids)).
		Where("po.campaign_type_phase_id IS NOT NULL").
		GroupExpr("po.campaign_id").
		Scan(ctx, &rows)
	if err != nil {
		return err
	}
	counts := make(map[string]int, len(rows))
	for _, row := range rows {
		counts[row.CampaignID] = row.N
	}
	for i := range campaigns {
		campaigns[i].PhasedPostCount = counts[campaigns[i].ID]
		campaigns[i].TypeLocked = campaigns[i].PhasedPostCount > 0
	}
	return nil
}

// hydratePhaseWindows loads the stored manual phase plan. Only GetByID hydrates
// it — every plan consumer (content_plan, the campaign assistant, reschedule,
// the phases endpoint) loads the campaign that way.
func (r *campaignRepository) hydratePhaseWindows(ctx context.Context, campaign *models.Campaign) error {
	var windows []models.CampaignPhaseWindow
	if err := r.db.NewSelect().
		Model(&windows).
		Where("cpw.campaign_id = ?", campaign.ID).
		OrderExpr("cpw.start_date ASC").
		Scan(ctx); err != nil {
		return err
	}
	campaign.PhaseWindows = windows
	return nil
}

func (r *campaignRepository) PhasePostCounts(ctx context.Context, campaignID string) (map[string]int, error) {
	var rows []struct {
		PhaseID string `bun:"phase_id"`
		N       int    `bun:"n"`
	}
	err := r.db.NewSelect().
		Model((*models.Post)(nil)).
		ColumnExpr("po.campaign_type_phase_id AS phase_id").
		ColumnExpr("count(*) AS n").
		Where("po.campaign_id = ?", campaignID).
		Where("po.campaign_type_phase_id IS NOT NULL").
		GroupExpr("po.campaign_type_phase_id").
		Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, row := range rows {
		out[row.PhaseID] = row.N
	}
	return out, nil
}

// ReplacePhaseWindows swaps the campaign's stored plan in one transaction, so a
// reader never sees a half-written (and therefore invalid) plan.
func (r *campaignRepository) ReplacePhaseWindows(ctx context.Context, campaignID string, windows []models.CampaignPhaseWindow) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().
			Model((*models.CampaignPhaseWindow)(nil)).
			Where("campaign_id = ?", campaignID).
			Exec(ctx); err != nil {
			return err
		}
		if len(windows) == 0 {
			return nil
		}
		now := time.Now().UTC()
		for i := range windows {
			windows[i].CampaignID = campaignID
			windows[i].CreatedAt = now
			windows[i].UpdatedAt = now
		}
		_, err := tx.NewInsert().Model(&windows).Exec(ctx)
		return err
	})
}

func (r *campaignRepository) DeletePhaseWindows(ctx context.Context, campaignID string) error {
	_, err := r.db.NewDelete().
		Model((*models.CampaignPhaseWindow)(nil)).
		Where("campaign_id = ?", campaignID).
		Exec(ctx)
	return err
}

func (r *campaignRepository) PhaseBelongsToCampaign(ctx context.Context, campaignID, phaseID string) (bool, error) {
	return r.db.NewSelect().
		Model((*models.Campaign)(nil)).
		Join("JOIN campaigns_types_phases AS ctp ON ctp.campaign_type_id = c.campaign_type_id").
		Where("c.id = ?", campaignID).
		Where("c.deleted_at IS NULL").
		Where("ctp.id = ?", phaseID).
		Exists(ctx)
}

// Constraint names raised by the CON-166 integrity triggers.
const (
	ConstraintPhaseMatchesCampaignType = "posts_phase_matches_campaign_type"
	ConstraintCampaignTypeLocked       = "campaigns_type_locked"
)

// IsConstraintViolation reports whether err is a Postgres error raised for the
// named constraint (a real constraint, or a trigger's CONSTRAINT = … tag).
func IsConstraintViolation(err error, constraint string) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.ConstraintName == constraint
}

// IsForeignKeyViolation reports whether err is a Postgres foreign-key violation
// (SQLSTATE 23503).
func IsForeignKeyViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23503"
}
