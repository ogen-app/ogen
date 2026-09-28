package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PostAnalyticsListOptions parameterises the list/overview query
// (CON-93 §5/§7). Publisher restricts the result to one publisher's
// posts (the analytics overview is "Zernio-published posts" today);
// empty means no publisher filter. Platform optionally narrows to one
// platform by name. SortBy/Order are validated/clamped by the caller and
// mapped to a whitelisted column here.
type PostAnalyticsListOptions struct {
	Publisher string
	Platform  string
	Page      int
	Limit     int
	SortBy    string
	Order     string
}

// PostAnalyticsListItem is one row of the overview, read from the current-state
// table (CON-236: one row per post, no latest-per-post subquery) and shaped to
// the §7 response contract (aggregate metrics nested under `analytics`).
type PostAnalyticsListItem struct {
	PostID             string                      `json:"post_id"`
	PublisherPostID    string                      `json:"publisher_post_id"`
	Title              string                      `json:"title"`
	Publisher          string                      `json:"publisher"`
	Platform           string                      `json:"platform"`
	PublishedAt        *time.Time                  `json:"published_at"`
	SyncStatus         string                      `json:"sync_status"`
	MetricsLastUpdated *time.Time                  `json:"metrics_last_updated"`
	LastRefreshedAt    time.Time                   `json:"last_refreshed_at"`
	Analytics          models.PostAnalyticsMetrics `json:"analytics"`
}

// PostAnalyticsOverview is the summed/averaged headline block over the
// full filtered set (not just the current page), per §7.
type PostAnalyticsOverview struct {
	PostCount         int     `json:"post_count"`
	Impressions       int     `json:"impressions"`
	Reach             int     `json:"reach"`
	Likes             int     `json:"likes"`
	Comments          int     `json:"comments"`
	Shares            int     `json:"shares"`
	Saves             int     `json:"saves"`
	Clicks            int     `json:"clicks"`
	Views             int     `json:"views"`
	EngagementRateAvg float64 `json:"engagement_rate_avg"`
}

// PostAnalyticsRepository persists analytics for Posts published through a
// publisher (CON-93; storage reworked in CON-236, isolated analytics DB). The
// current state is ONE upserted row per post (post_analytics_current); the
// append-only trend history is a separate table written only on change
// (post_analytics_snapshots). Reads serve entirely from the current-state row.
type PostAnalyticsRepository interface {
	// Upsert writes the current state for a post, keyed on (tenant_id, post_id):
	// the per-check write refreshes the metrics/display fields in place rather
	// than appending. tenant_id is stamped by the TenantScoped hook. Used on the
	// unchanged path (bump last_checked_at only).
	Upsert(ctx context.Context, a *models.PostAnalytics) error
	// AppendSnapshot appends one point to the trend history (CON-236: only when
	// the metrics changed). ID + OccurredAt are set by the caller.
	AppendSnapshot(ctx context.Context, s *models.PostAnalyticsSnapshot) error
	// UpsertWithSnapshot writes the current-state row and appends the trend point
	// in ONE transaction (CON-236): on a changed refresh both commit together, so
	// a snapshot failure can't leave the current row advanced without its history
	// point (which dedup would then hide forever). Use on the changed path.
	UpsertWithSnapshot(ctx context.Context, a *models.PostAnalytics, s *models.PostAnalyticsSnapshot) error
	// GetByPostID returns the current-state row for a post, or (nil, nil) when
	// the post has no snapshot yet (refresh hasn't covered it).
	GetByPostID(ctx context.Context, postID string) (*models.PostAnalytics, error)
	// CurrentByPostID returns the current-state rows for the ctx tenant keyed by
	// post_id, so the refresh job can dedup (compare metric keys) and apply the
	// decay schedule (last_checked_at) without a per-post query.
	CurrentByPostID(ctx context.Context) (map[string]*models.PostAnalytics, error)
	// List returns one page of current-state rows plus the overview computed over
	// the full filtered set and the total count.
	List(ctx context.Context, opts PostAnalyticsListOptions) ([]PostAnalyticsListItem, PostAnalyticsOverview, error)
	// PublishedBetween returns the current-state rows whose post was published in
	// [from, to) — the analytics side of the CON-237 overview (reach/interactions
	// attributed to publish day). Ordered by published_at. Tenant-scoped.
	PublishedBetween(ctx context.Context, from, to time.Time) ([]models.PostAnalytics, error)
	// ReachByAgeSamples returns per-(platform, post, age-day) metric observations
	// for the CON-238 per-platform expected-at-age baseline curve: it joins the
	// slim history (post_analytics_snapshots) to the current row for published_at
	// and platform, and takes the max metric within each age-day (both monotonic).
	// Tenant-scoped. Bounded per-tenant volume — the seam where a continuous
	// aggregate can replace the raw scan later. CON-250 widened the projection to
	// impressions/saves/clicks so the same samples feed the per-post p25/p50/p75
	// "usual range" band across all six card metrics (performers reads only
	// reach/interactions and ignores the rest).
	ReachByAgeSamples(ctx context.Context) ([]ReachAgeSample, error)
	// SnapshotsByPostID returns one post's full deduped trend history (all metric
	// columns + occurred_at), ascending. The per-post running-total series
	// (CON-250) is built from these change points carried forward. Tenant-scoped
	// (the snapshot model's hook scopes to the ctx tenant); bounded to one post.
	SnapshotsByPostID(ctx context.Context, postID string) ([]models.PostAnalyticsSnapshot, error)
	// LifespanSamples returns per-(post, age-hour) reach observations for the
	// CON-239 post-lifespan curve: the slim history joined to the current row for
	// published_at, max reach per age-hour (hour resolution captures t50 ≈ 19h;
	// CON-236's hourly fresh-post decay makes that granularity real). Workspace-
	// wide (no platform split). A non-zero `since` restricts to posts published on
	// or after it (matching the heatmap/patterns scope); zero = all-time.
	// Tenant-scoped.
	LifespanSamples(ctx context.Context, since time.Time) ([]LifespanSample, error)
}

// ReachAgeSample is one historical (platform, post, age-day) observation for the
// CON-238 accrual-curve baseline. Reach/Interactions drive the CON-238
// multiplier; Impressions/Saves/Clicks were added for CON-250's per-metric band.
type ReachAgeSample struct {
	Platform     string `bun:"platform"`
	PostID       string `bun:"post_id"`
	AgeDay       int    `bun:"age_day"`
	Reach        int    `bun:"reach"`
	Interactions int    `bun:"interactions"`
	Impressions  int    `bun:"impressions"`
	Saves        int    `bun:"saves"`
	Clicks       int    `bun:"clicks"`
}

// LifespanSample is one (post, age-hour) reach observation for the CON-239
// post-lifespan curve.
type LifespanSample struct {
	PostID   string `bun:"post_id"`
	AgeHours int    `bun:"age_hours"`
	Reach    int    `bun:"reach"`
}

type postAnalyticsRepository struct {
	db *bun.DB
}

// NewPostAnalyticsRepository builds a PostAnalyticsRepository. Pass the
// analytics *bun.DB (post_analytics_current / post_analytics_snapshots live there).
func NewPostAnalyticsRepository(db *bun.DB) PostAnalyticsRepository {
	return &postAnalyticsRepository{db: db}
}

// PublishedBetween returns current-state rows whose published_at falls in
// [from, to). The TenantScoped hook adds the tenant predicate. Post counts per
// tenant are modest, so this is a plain indexed scan of post_analytics_current.
func (r *postAnalyticsRepository) PublishedBetween(ctx context.Context, from, to time.Time) ([]models.PostAnalytics, error) {
	var rows []models.PostAnalytics
	if err := r.db.NewSelect().Model(&rows).
		Where("pa.published_at >= ?", from).
		Where("pa.published_at < ?", to).
		OrderExpr("pa.published_at ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return rows, nil
}

// ReachByAgeSamples joins the slim history to the current row and reduces to one
// (platform, post, age-day) row carrying the max reach/interactions seen in that
// age-day. The snapshot model's TenantScoped hook scopes `pas`; the join keeps
// `pac` on the same tenant. age_day = whole days since publish.
func (r *postAnalyticsRepository) ReachByAgeSamples(ctx context.Context) ([]ReachAgeSample, error) {
	const ageExpr = "FLOOR(EXTRACT(EPOCH FROM (pas.occurred_at - pac.published_at)) / 86400)::int"
	var out []ReachAgeSample
	if err := r.db.NewSelect().Model((*models.PostAnalyticsSnapshot)(nil)).
		Join("JOIN post_analytics_current AS pac ON pac.tenant_id = pas.tenant_id AND pac.post_id = pas.post_id").
		ColumnExpr("pac.platform AS platform").
		ColumnExpr("pas.post_id AS post_id").
		ColumnExpr(ageExpr+" AS age_day").
		ColumnExpr("MAX(pas.reach) AS reach").
		ColumnExpr("MAX(pas.likes + pas.comments + pas.shares) AS interactions").
		ColumnExpr("MAX(pas.impressions) AS impressions").
		ColumnExpr("MAX(pas.saves) AS saves").
		ColumnExpr("MAX(pas.clicks) AS clicks").
		Where("pac.published_at IS NOT NULL").
		Where("pas.occurred_at >= pac.published_at").
		GroupExpr("pac.platform, pas.post_id, "+ageExpr).
		Scan(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SnapshotsByPostID returns one post's deduped trend history, ascending by
// occurred_at. The TenantScoped hook on the snapshot model restricts to the ctx
// tenant, so a cross-tenant post_id yields an empty slice.
func (r *postAnalyticsRepository) SnapshotsByPostID(ctx context.Context, postID string) ([]models.PostAnalyticsSnapshot, error) {
	var rows []models.PostAnalyticsSnapshot
	if err := r.db.NewSelect().Model(&rows).
		Where("pas.post_id = ?", postID).
		OrderExpr("pas.occurred_at ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return rows, nil
}

// LifespanSamples joins the slim history to the current row and reduces to one
// (post, age-hour) row with the max reach in that hour. Same tenant-scoping as
// ReachByAgeSamples; hour resolution rather than day, and no platform split.
func (r *postAnalyticsRepository) LifespanSamples(ctx context.Context, since time.Time) ([]LifespanSample, error) {
	const ageExpr = "FLOOR(EXTRACT(EPOCH FROM (pas.occurred_at - pac.published_at)) / 3600)::int"
	var out []LifespanSample
	q := r.db.NewSelect().Model((*models.PostAnalyticsSnapshot)(nil)).
		Join("JOIN post_analytics_current AS pac ON pac.tenant_id = pas.tenant_id AND pac.post_id = pas.post_id").
		ColumnExpr("pas.post_id AS post_id").
		ColumnExpr(ageExpr + " AS age_hours").
		ColumnExpr("MAX(pas.reach) AS reach").
		Where("pac.published_at IS NOT NULL").
		Where("pas.occurred_at >= pac.published_at").
		GroupExpr("pas.post_id, " + ageExpr)
	if !since.IsZero() {
		q = q.Where("pac.published_at >= ?", since)
	}
	if err := q.Scan(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *postAnalyticsRepository) Upsert(ctx context.Context, a *models.PostAnalytics) error {
	_, err := upsertCurrentQuery(r.db, a).Exec(ctx)
	return err
}

// upsertCurrentQuery builds the (tenant_id, post_id) current-state upsert on the
// given db/tx. tenant_id is stamped per row by the TenantScoped BeforeAppendModel
// hook, so the conflict target always has a value.
func upsertCurrentQuery(idb bun.IDB, a *models.PostAnalytics) *bun.InsertQuery {
	return idb.NewInsert().Model(a).
		On("CONFLICT (tenant_id, post_id) DO UPDATE").
		Set("publisher = EXCLUDED.publisher").
		Set("publisher_post_id = EXCLUDED.publisher_post_id").
		Set("platform = EXCLUDED.platform").
		Set("title = EXCLUDED.title").
		Set("published_at = EXCLUDED.published_at").
		Set("impressions = EXCLUDED.impressions").
		Set("reach = EXCLUDED.reach").
		Set("likes = EXCLUDED.likes").
		Set("comments = EXCLUDED.comments").
		Set("shares = EXCLUDED.shares").
		Set("saves = EXCLUDED.saves").
		Set("clicks = EXCLUDED.clicks").
		Set("views = EXCLUDED.views").
		Set("engagement_rate = EXCLUDED.engagement_rate").
		Set("platform_analytics = EXCLUDED.platform_analytics").
		Set("sync_status = EXCLUDED.sync_status").
		Set("metrics_last_updated = EXCLUDED.metrics_last_updated").
		Set("last_changed_at = EXCLUDED.last_changed_at").
		Set("last_checked_at = EXCLUDED.last_checked_at")
}

func (r *postAnalyticsRepository) AppendSnapshot(ctx context.Context, s *models.PostAnalyticsSnapshot) error {
	_, err := r.db.NewInsert().Model(s).Exec(ctx)
	return err
}

func (r *postAnalyticsRepository) UpsertWithSnapshot(ctx context.Context, a *models.PostAnalytics, s *models.PostAnalyticsSnapshot) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := upsertCurrentQuery(tx, a).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewInsert().Model(s).Exec(ctx); err != nil {
			return err
		}
		return nil
	})
}

func (r *postAnalyticsRepository) GetByPostID(ctx context.Context, postID string) (*models.PostAnalytics, error) {
	a := new(models.PostAnalytics)
	err := r.db.NewSelect().
		Model(a).
		Where("pa.post_id = ?", postID).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return a, nil
}

func (r *postAnalyticsRepository) CurrentByPostID(ctx context.Context) (map[string]*models.PostAnalytics, error) {
	var rows []models.PostAnalytics
	// TenantScoped hook restricts to the ctx tenant.
	if err := r.db.NewSelect().Model(&rows).Scan(ctx); err != nil {
		return nil, err
	}
	out := make(map[string]*models.PostAnalytics, len(rows))
	for i := range rows {
		out[rows[i].PostID] = &rows[i]
	}
	return out, nil
}

// postAnalyticsSortColumns whitelists the §5 sort_by values, mapping each to a
// fully-qualified column on the current-state table. Keeping the map closed
// prevents arbitrary expressions reaching ORDER BY.
var postAnalyticsSortColumns = map[string]string{
	"engagement":   "pa.engagement_rate",
	"impressions":  "pa.impressions",
	"reach":        "pa.reach",
	"likes":        "pa.likes",
	"comments":     "pa.comments",
	"shares":       "pa.shares",
	"saves":        "pa.saves",
	"clicks":       "pa.clicks",
	"views":        "pa.views",
	"published_at": "pa.published_at",
}

// postAnalyticsListRow is the flat scan target for the current-state read.
type postAnalyticsListRow struct {
	PostID             string     `bun:"post_id"`
	PublisherPostID    string     `bun:"publisher_post_id"`
	Title              string     `bun:"title"`
	Publisher          string     `bun:"publisher"`
	Platform           string     `bun:"platform"`
	PublishedAt        *time.Time `bun:"published_at"`
	SyncStatus         string     `bun:"sync_status"`
	MetricsLastUpdated *time.Time `bun:"metrics_last_updated"`
	LastRefreshedAt    time.Time  `bun:"last_refreshed_at"`
	Impressions        int        `bun:"impressions"`
	Reach              int        `bun:"reach"`
	Likes              int        `bun:"likes"`
	Comments           int        `bun:"comments"`
	Shares             int        `bun:"shares"`
	Saves              int        `bun:"saves"`
	Clicks             int        `bun:"clicks"`
	Views              int        `bun:"views"`
	EngagementRate     float64    `bun:"engagement_rate"`
}

func (r *postAnalyticsRepository) List(ctx context.Context, opts PostAnalyticsListOptions) ([]PostAnalyticsListItem, PostAnalyticsOverview, error) {
	page := max(opts.Page, 1)
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	sortCol, ok := postAnalyticsSortColumns[opts.SortBy]
	if !ok {
		sortCol = postAnalyticsSortColumns["engagement"]
	}
	order := "DESC"
	if opts.Order == "asc" {
		order = "ASC"
	}

	// filtered builds a fresh Model query over the current-state table with the
	// publisher / platform filters. It stays a Model query so the TenantScoped
	// hook adds `pa.tenant_id = <ctx tenant>`. One row per post already, so no
	// latest-per-post subquery is needed (CON-236).
	filtered := func() *bun.SelectQuery {
		q := r.db.NewSelect().Model((*models.PostAnalytics)(nil))
		if opts.Publisher != "" {
			q = q.Where("pa.publisher = ?", opts.Publisher)
		}
		if opts.Platform != "" {
			q = q.Where("pa.platform = ?", opts.Platform)
		}
		return q
	}

	// Overview + total over the full filtered set.
	var overview PostAnalyticsOverview
	err := filtered().
		ColumnExpr("COUNT(*) AS post_count").
		ColumnExpr("COALESCE(SUM(pa.impressions), 0) AS impressions").
		ColumnExpr("COALESCE(SUM(pa.reach), 0) AS reach").
		ColumnExpr("COALESCE(SUM(pa.likes), 0) AS likes").
		ColumnExpr("COALESCE(SUM(pa.comments), 0) AS comments").
		ColumnExpr("COALESCE(SUM(pa.shares), 0) AS shares").
		ColumnExpr("COALESCE(SUM(pa.saves), 0) AS saves").
		ColumnExpr("COALESCE(SUM(pa.clicks), 0) AS clicks").
		ColumnExpr("COALESCE(SUM(pa.views), 0) AS views").
		ColumnExpr("COALESCE(AVG(pa.engagement_rate), 0) AS engagement_rate_avg").
		Scan(ctx, &overview)
	if err != nil {
		return nil, PostAnalyticsOverview{}, err
	}

	if overview.PostCount == 0 {
		return []PostAnalyticsListItem{}, overview, nil
	}

	var rows []postAnalyticsListRow
	err = filtered().
		ColumnExpr("pa.post_id, pa.publisher_post_id, pa.sync_status, pa.metrics_last_updated, pa.last_checked_at AS last_refreshed_at").
		ColumnExpr("pa.impressions, pa.reach, pa.likes, pa.comments, pa.shares, pa.saves, pa.clicks, pa.views, pa.engagement_rate").
		ColumnExpr("pa.title AS title, pa.publisher AS publisher, pa.published_at AS published_at").
		ColumnExpr("COALESCE(pa.platform, '') AS platform").
		OrderExpr(sortCol+" "+order).
		// Stable tiebreaker so paging is deterministic when the sort key ties.
		OrderExpr("pa.post_id ASC").
		Limit(limit).
		Offset((page-1)*limit).
		Scan(ctx, &rows)
	if err != nil {
		return nil, PostAnalyticsOverview{}, err
	}

	items := make([]PostAnalyticsListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, PostAnalyticsListItem{
			PostID:             row.PostID,
			PublisherPostID:    row.PublisherPostID,
			Title:              row.Title,
			Publisher:          row.Publisher,
			Platform:           row.Platform,
			PublishedAt:        row.PublishedAt,
			SyncStatus:         row.SyncStatus,
			MetricsLastUpdated: row.MetricsLastUpdated,
			LastRefreshedAt:    row.LastRefreshedAt,
			Analytics: models.PostAnalyticsMetrics{
				Impressions:    row.Impressions,
				Reach:          row.Reach,
				Likes:          row.Likes,
				Comments:       row.Comments,
				Shares:         row.Shares,
				Saves:          row.Saves,
				Clicks:         row.Clicks,
				Views:          row.Views,
				EngagementRate: row.EngagementRate,
			},
		})
	}
	return items, overview, nil
}
