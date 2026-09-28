package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// FollowerAccountSummary is the latest snapshot per account plus that
// account's data-point count, for the followers overview.
type FollowerAccountSummary struct {
	SocialAccountID  string  `bun:"social_account_id" json:"account_id"`
	Platform         string  `bun:"platform"          json:"platform"`
	Username         string  `bun:"username"          json:"username"`
	CurrentFollowers int64   `bun:"followers"         json:"current_followers"`
	Growth           int64   `bun:"growth"            json:"growth"`
	GrowthPercentage float64 `bun:"growth_percentage" json:"growth_percentage"`
	DataPoints       int     `bun:"data_points"       json:"data_points"`
}

// FollowerSeriesPoint is one datum in an account's follower time series.
type FollowerSeriesPoint struct {
	SocialAccountID string    `bun:"social_account_id" json:"-"`
	PointDate       time.Time `bun:"point_date"        json:"date"`
	Followers       int64     `bun:"followers"         json:"followers"`
}

// FollowerSeriesOptions filters the series read. Zero fields are omitted.
type FollowerSeriesOptions struct {
	AccountID string    // optional: restrict to one account
	From      time.Time // inclusive lower bound on point_date; zero = unbounded
	To        time.Time // inclusive upper bound on point_date; zero = unbounded
}

// FollowerStatsRepository persists and reads the daily follower time series
// from the isolated analytics DB. Writes upsert one row per
// (account, day); reads are tenant-scoped by the TenantScoped hook.
type FollowerStatsRepository interface {
	// InsertMany upserts a batch of daily points, keyed on
	// (tenant_id, social_account_id, point_date): a same-day re-fetch refines
	// the row in place rather than appending a duplicate. Returns the number of
	// rows inserted or updated.
	InsertMany(ctx context.Context, rows []models.FollowerSnapshot) (int, error)
	// Summary returns the latest snapshot per account (optionally one account),
	// with each account's total data-point count.
	Summary(ctx context.Context, accountID string) ([]FollowerAccountSummary, error)
	// Series returns the follower points matching opts, ordered by
	// (account, point_date ASC).
	Series(ctx context.Context, opts FollowerSeriesOptions) ([]FollowerSeriesPoint, error)
	// LatestPointDate returns the newest point_date recorded for the current
	// tenant (zero time when none), so the refresh job can request only newer
	// points from Zernio.
	LatestPointDate(ctx context.Context) (time.Time, error)
}

type followerStatsRepository struct {
	db *bun.DB
}

// NewFollowerStatsRepository builds a FollowerStatsRepository. Pass the
// analytics *bun.DB (follower_stats_snapshots lives there).
func NewFollowerStatsRepository(db *bun.DB) FollowerStatsRepository {
	return &followerStatsRepository{db: db}
}

func (r *followerStatsRepository) InsertMany(ctx context.Context, rows []models.FollowerSnapshot) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	// tenant_id is stamped per row by the TenantScoped BeforeAppendModel hook
	// from the write context, so the conflict target below always has a value.
	res, err := r.db.NewInsert().Model(&rows).
		On("CONFLICT (tenant_id, social_account_id, point_date) DO UPDATE").
		Set("followers = EXCLUDED.followers").
		Set("growth = EXCLUDED.growth").
		Set("growth_percentage = EXCLUDED.growth_percentage").
		Set("platform = EXCLUDED.platform").
		Set("username = EXCLUDED.username").
		Set("raw_json = EXCLUDED.raw_json").
		Set("recorded_at = EXCLUDED.recorded_at").
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (r *followerStatsRepository) Summary(ctx context.Context, accountID string) ([]FollowerAccountSummary, error) {
	// DISTINCT ON (social_account_id) ordered by point_date DESC yields the
	// latest row per account; the COUNT(*) OVER window is constant within each
	// account partition, so the retained row carries the right data-point count.
	// The TenantScoped hook adds `fs.tenant_id = <ctx>` to the WHERE.
	q := r.db.NewSelect().Model((*models.FollowerSnapshot)(nil)).
		ColumnExpr("DISTINCT ON (fs.social_account_id) fs.social_account_id").
		ColumnExpr("fs.platform, fs.username, fs.followers, fs.growth, fs.growth_percentage").
		ColumnExpr("COUNT(*) OVER (PARTITION BY fs.social_account_id) AS data_points").
		OrderExpr("fs.social_account_id, fs.point_date DESC")
	if accountID != "" {
		q = q.Where("fs.social_account_id = ?", accountID)
	}
	var out []FollowerAccountSummary
	if err := q.Scan(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *followerStatsRepository) Series(ctx context.Context, opts FollowerSeriesOptions) ([]FollowerSeriesPoint, error) {
	q := r.db.NewSelect().Model((*models.FollowerSnapshot)(nil)).
		ColumnExpr("fs.social_account_id, fs.point_date, fs.followers").
		OrderExpr("fs.social_account_id, fs.point_date ASC")
	if opts.AccountID != "" {
		q = q.Where("fs.social_account_id = ?", opts.AccountID)
	}
	if !opts.From.IsZero() {
		q = q.Where("fs.point_date >= ?", opts.From)
	}
	if !opts.To.IsZero() {
		q = q.Where("fs.point_date <= ?", opts.To)
	}
	var out []FollowerSeriesPoint
	if err := q.Scan(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *followerStatsRepository) LatestPointDate(ctx context.Context) (time.Time, error) {
	// MAX over an empty set is NULL; scan through NullTime so "no rows yet"
	// returns the zero time rather than a scan error.
	var latest sql.NullTime
	err := r.db.NewSelect().Model((*models.FollowerSnapshot)(nil)).
		ColumnExpr("MAX(fs.point_date)").
		Scan(ctx, &latest)
	if err != nil {
		return time.Time{}, err
	}
	if !latest.Valid {
		return time.Time{}, nil
	}
	return latest.Time, nil
}
