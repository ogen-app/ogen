package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// UsageRepository persists and reads vendor_usage_events in the isolated analytics
// database. It is constructed with the analytics *bun.DB, not the
// main pool. Writes come from the async usage.Recorder in a system context
// (tenant pre-set per row); reads run in the caller's tenant context and are
// auto-scoped by the UsageEvent TenantScoped hooks.
type UsageRepository interface {
	// Insert writes a batch of events. A nil/empty batch is a no-op.
	Insert(ctx context.Context, events []*models.UsageEvent) error

	// SpendBetween returns the calling tenant's total cost_micros for events
	// in [start, end). Used by enforcement (period-to-date spend) and the
	// usage summary totals.
	SpendBetween(ctx context.Context, start, end time.Time) (int64, error)

	// Summary returns the calling tenant's per-dimension aggregates for
	// events in [start, end), one row per
	// vendor×family×model×operation×feature×platform. Used by GET /api/usage.
	Summary(ctx context.Context, start, end time.Time) ([]UsageBreakdownRow, error)
}

// UsageBreakdownRow is one grouped aggregate of usage. The common token kinds
// are summed into columns; the long-tail kinds (embed_input, reasoning, and the
// publisher kinds post/api_call/account) are summed per kind into ExtraUnits so
// embedding/publisher usage is visible, not just its cost. Cost and event count
// accompany them.
type UsageBreakdownRow struct {
	Vendor              string           `bun:"vendor"             json:"vendor"`
	Family              string           `bun:"family"             json:"family"`
	Model               string           `bun:"model"              json:"model"`
	Operation           string           `bun:"operation"          json:"operation"`
	Feature             string           `bun:"feature"            json:"feature"`
	Platform            string           `bun:"platform"           json:"platform,omitempty"`
	InputTokens         int64            `bun:"input_tokens"       json:"input_tokens"`
	OutputTokens        int64            `bun:"output_tokens"      json:"output_tokens"`
	CacheReadTokens     int64            `bun:"cache_read_tokens"  json:"cache_read_tokens"`
	CacheCreationTokens int64            `bun:"cache_creation_tokens" json:"cache_creation_tokens"`
	CostMicros          int64            `bun:"cost_micros"        json:"cost_micros"`
	Count               int64            `bun:"count"              json:"count"`
	ExtraUnits          map[string]int64 `bun:"-"                  json:"extra_units,omitempty"`
}

type usageRepository struct {
	db *bun.DB
}

// NewUsageRepository builds a UsageRepository. Pass the analytics *bun.DB.
func NewUsageRepository(db *bun.DB) UsageRepository {
	return &usageRepository{db: db}
}

func (r *usageRepository) Insert(ctx context.Context, events []*models.UsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	_, err := r.db.NewInsert().Model(&events).Exec(ctx)
	return err
}

func (r *usageRepository) SpendBetween(ctx context.Context, start, end time.Time) (int64, error) {
	var total int64
	err := r.db.NewSelect().
		Model((*models.UsageEvent)(nil)).
		ColumnExpr("COALESCE(SUM(cost_micros), 0)").
		Where("occurred_at >= ?", start).
		Where("occurred_at < ?", end).
		Scan(ctx, &total)
	if err != nil {
		return 0, err
	}
	return total, nil
}

func (r *usageRepository) Summary(ctx context.Context, start, end time.Time) ([]UsageBreakdownRow, error) {
	var rows []UsageBreakdownRow
	err := r.db.NewSelect().
		Model((*models.UsageEvent)(nil)).
		ColumnExpr("vendor, family, model, operation, feature, COALESCE(platform, '') AS platform").
		ColumnExpr("SUM(input_tokens) AS input_tokens").
		ColumnExpr("SUM(output_tokens) AS output_tokens").
		ColumnExpr("SUM(cache_read_tokens) AS cache_read_tokens").
		ColumnExpr("SUM(cache_creation_tokens) AS cache_creation_tokens").
		ColumnExpr("SUM(cost_micros) AS cost_micros").
		ColumnExpr("COUNT(*) AS count").
		Where("occurred_at >= ?", start).
		Where("occurred_at < ?", end).
		GroupExpr("vendor, family, model, operation, feature, COALESCE(platform, '')").
		OrderExpr("cost_micros DESC").
		Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return rows, nil
	}

	// Second pass: sum the long-tail extra_units jsonb per kind, grouped the
	// same way, and fold it back onto each breakdown row. Done separately
	// because unnesting the jsonb in the main query would multiply rows and
	// inflate the column SUMs/COUNT. Kept generic (any kind key) so a new
	// vendor's units surface without a code change — same open-set principle as
	// the storage layer. The CROSS JOIN LATERAL drops rows with null/empty
	// extra_units, so model-only events cost nothing here.
	var extras []struct {
		Vendor    string `bun:"vendor"`
		Family    string `bun:"family"`
		Model     string `bun:"model"`
		Operation string `bun:"operation"`
		Feature   string `bun:"feature"`
		Platform  string `bun:"platform"`
		Kind      string `bun:"kind"`
		Units     int64  `bun:"units"`
	}
	err = r.db.NewSelect().
		Model((*models.UsageEvent)(nil)).
		ColumnExpr("vendor, family, model, operation, feature, COALESCE(platform, '') AS platform").
		ColumnExpr("kv.key AS kind").
		ColumnExpr("SUM(kv.value::bigint) AS units").
		Join("CROSS JOIN LATERAL jsonb_each_text(ue.extra_units) AS kv").
		Where("occurred_at >= ?", start).
		Where("occurred_at < ?", end).
		GroupExpr("vendor, family, model, operation, feature, COALESCE(platform, ''), kv.key").
		Scan(ctx, &extras)
	if err != nil {
		return nil, err
	}

	groupKey := func(vendor, family, model, op, feature, platform string) string {
		return vendor + "\x1f" + family + "\x1f" + model + "\x1f" + op + "\x1f" + feature + "\x1f" + platform
	}
	idx := make(map[string]int, len(rows))
	for i := range rows {
		idx[groupKey(rows[i].Vendor, rows[i].Family, rows[i].Model, rows[i].Operation, rows[i].Feature, rows[i].Platform)] = i
	}
	for _, e := range extras {
		i, ok := idx[groupKey(e.Vendor, e.Family, e.Model, e.Operation, e.Feature, e.Platform)]
		if !ok {
			continue // every extra group has a base row, but never panic if not
		}
		if rows[i].ExtraUnits == nil {
			rows[i].ExtraUnits = make(map[string]int64)
		}
		rows[i].ExtraUnits[e.Kind] = e.Units
	}
	return rows, nil
}
