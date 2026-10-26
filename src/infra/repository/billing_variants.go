package repository

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
)

// ErrVariantTaken is returned when a variant already sells another price in
// the same mode.
var ErrVariantTaken = errors.New("variant already sells another price")

const variantMappingUnique = "uq_billing_variant_mappings_variant"

func (r *billingRepository) ListVariantMappings(ctx context.Context, testMode bool) ([]models.BillingVariantMapping, error) {
	var rows []models.BillingVariantMapping
	err := r.db.NewSelect().Model(&rows).
		Where("bvm.test_mode = ?", testMode).
		OrderExpr("bvm.tier_version_price_id").
		Scan(ctx)
	return rows, err
}

func (r *billingRepository) UpsertVariantMapping(ctx context.Context, m *models.BillingVariantMapping) error {
	_, err := r.db.NewInsert().Model(m).
		On("CONFLICT (tier_version_price_id, test_mode) DO UPDATE").
		Set("variant_id = EXCLUDED.variant_id").
		Set("updated_at = EXCLUDED.updated_at").
		Set("updated_by = EXCLUDED.updated_by").
		Set("reason = EXCLUDED.reason").
		Exec(ctx)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" && pgErr.ConstraintName == variantMappingUnique {
		return ErrVariantTaken
	}
	return err
}

func (r *billingRepository) DeleteVariantMapping(ctx context.Context, priceID string, testMode bool) (bool, error) {
	res, err := r.db.NewDelete().Model((*models.BillingVariantMapping)(nil)).
		Where("tier_version_price_id = ?", priceID).
		Where("test_mode = ?", testMode).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (r *billingRepository) SeedVariantMappings(ctx context.Context, testMode bool, rows []models.BillingVariantMapping) (int, []string, error) {
	inserted := 0
	var unknown []string
	err := r.db.RunInTx(ctx, &sql.TxOptions{}, func(ctx context.Context, tx bun.Tx) error {
		// Replicas boot together: the lock makes the emptiness check and the
		// insert one step, so exactly one of them seeds.
		if _, err := tx.NewRaw("SELECT pg_advisory_xact_lock(hashtext('billing_variant_seed'))").Exec(ctx); err != nil {
			return err
		}
		exists, err := tx.NewSelect().Model((*models.BillingVariantMapping)(nil)).Where("test_mode = ?", testMode).Exists(ctx)
		if err != nil || exists {
			return err
		}
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.TierVersionPriceID)
		}
		var known []string
		if len(ids) > 0 {
			if err := tx.NewSelect().TableExpr("tenant_tier_version_prices").Column("id").
				Where("id IN (?)", bun.List(ids)).Scan(ctx, &known); err != nil {
				return err
			}
		}
		var keep []models.BillingVariantMapping
		for _, row := range rows {
			if !slices.Contains(known, row.TierVersionPriceID) {
				unknown = append(unknown, row.TierVersionPriceID)
				continue
			}
			row.TestMode = testMode
			keep = append(keep, row)
		}
		if len(keep) == 0 {
			return nil
		}
		if _, err := tx.NewInsert().Model(&keep).Exec(ctx); err != nil {
			return err
		}
		inserted = len(keep)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	slices.Sort(unknown)
	return inserted, unknown, nil
}

func (r *billingRepository) MappablePrices(ctx context.Context, extraIDs []string) ([]billing.Price, error) {
	q := r.priceQuery().
		WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			q = q.Where("v.status = ? AND v.purchasable AND p.net_minor > 0", models.TierVersionStatusActive)
			if len(extraIDs) > 0 {
				q = q.WhereOr("p.id IN (?)", bun.List(extraIDs))
			}
			return q
		}).
		OrderExpr("tt.name, v.version, p.currency, p.billing_interval, p.country_code NULLS FIRST")
	return scanPrices(ctx, q)
}

func (r *billingRepository) Prices(ctx context.Context, ids []string) (map[string]billing.Price, error) {
	out := map[string]billing.Price{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := scanPrices(ctx, r.priceQuery().Where("p.id IN (?)", bun.List(ids)))
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		out[p.ID] = p
	}
	return out, nil
}

// priceQuery selects price rows with their tier and version.
func (r *billingRepository) priceQuery() *bun.SelectQuery {
	return r.db.NewSelect().
		TableExpr("tenant_tier_version_prices AS p").
		Join("JOIN tenant_tier_versions AS v ON v.id = p.tier_version_id").
		Join("JOIN tenant_tiers AS tt ON tt.id = v.tier_id").
		ColumnExpr("p.id, v.tier_id, tt.name AS tier_name, p.tier_version_id, v.version, p.currency, p.billing_interval, p.net_minor, COALESCE(p.country_code, '') AS country_code, COALESCE(v.published_at, v.created_at) AS published_at")
}

func scanPrices(ctx context.Context, q *bun.SelectQuery) ([]billing.Price, error) {
	var rows []struct {
		ID            string    `bun:"id"`
		TierID        string    `bun:"tier_id"`
		TierName      string    `bun:"tier_name"`
		TierVersionID string    `bun:"tier_version_id"`
		Version       int       `bun:"version"`
		Currency      string    `bun:"currency"`
		Interval      string    `bun:"billing_interval"`
		NetMinor      int64     `bun:"net_minor"`
		CountryCode   string    `bun:"country_code"`
		PublishedAt   time.Time `bun:"published_at"`
	}
	if err := q.Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]billing.Price, 0, len(rows))
	for _, row := range rows {
		out = append(out, billing.Price{
			ID: row.ID, TierID: row.TierID, TierName: row.TierName, TierVersionID: row.TierVersionID, TierVersion: row.Version,
			Currency: row.Currency, Interval: row.Interval, NetMinor: row.NetMinor, CountryCode: row.CountryCode, PublishedAt: row.PublishedAt,
		})
	}
	return out, nil
}
