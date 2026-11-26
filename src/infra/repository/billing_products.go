package repository

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
)

// ErrProductTaken is returned when a product already sells another price in
// the same currency and mode.
var ErrProductTaken = errors.New("product already sells another price")

const productMappingUnique = "uq_billing_product_mappings_product"

func (r *billingRepository) ListProductMappings(ctx context.Context, testMode bool) ([]models.BillingProductMapping, error) {
	var rows []models.BillingProductMapping
	err := r.db.NewSelect().Model(&rows).
		Where("bpm.test_mode = ?", testMode).
		OrderExpr("bpm.tier_version_price_id").
		Scan(ctx)
	return rows, err
}

func (r *billingRepository) UpsertProductMapping(ctx context.Context, m *models.BillingProductMapping) error {
	_, err := r.db.NewInsert().Model(m).
		On("CONFLICT (tier_version_price_id, test_mode) DO UPDATE").
		Set("product_id = EXCLUDED.product_id").
		Set("currency = EXCLUDED.currency").
		Set("updated_at = EXCLUDED.updated_at").
		Set("updated_by = EXCLUDED.updated_by").
		Set("reason = EXCLUDED.reason").
		Exec(ctx)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" && pgErr.ConstraintName == productMappingUnique {
		return ErrProductTaken
	}
	return err
}

func (r *billingRepository) DeleteProductMapping(ctx context.Context, priceID string, testMode bool) (bool, error) {
	res, err := r.db.NewDelete().Model((*models.BillingProductMapping)(nil)).
		Where("tier_version_price_id = ?", priceID).
		Where("test_mode = ?", testMode).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
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

func (r *billingRepository) UpsertFXRates(ctx context.Context, rates []billing.FXRate) error {
	if len(rates) == 0 {
		return nil
	}
	rows := make([]models.FXRate, 0, len(rates))
	for _, fx := range rates {
		rows = append(rows, models.FXRate{RateDate: fx.Date, Currency: strings.ToUpper(fx.Currency), PerEUR: fx.PerEUR.FloatString(10), FetchedAt: time.Now().UTC()})
	}
	_, err := r.db.NewInsert().Model(&rows).
		On("CONFLICT (rate_date, currency) DO UPDATE").
		Set("per_eur = EXCLUDED.per_eur").
		Set("fetched_at = EXCLUDED.fetched_at").
		Exec(ctx)
	return err
}

func (r *billingRepository) FXRates(ctx context.Context, currencies []string, from time.Time) ([]billing.FXRate, error) {
	want := []string{"USD"}
	for _, c := range currencies {
		if c = strings.ToUpper(c); !slices.Contains(want, c) {
			want = append(want, c)
		}
	}
	var rows []models.FXRate
	if err := r.db.NewSelect().Model(&rows).
		Where("fx.currency IN (?)", bun.List(want)).
		Where("fx.rate_date >= ?", from).
		OrderExpr("fx.rate_date").
		Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]billing.FXRate, 0, len(rows))
	for _, row := range rows {
		per, err := billing.ParseRate(row.PerEUR)
		if err != nil {
			return nil, err
		}
		out = append(out, billing.FXRate{Date: row.RateDate, Currency: row.Currency, PerEUR: per})
	}
	return out, nil
}
