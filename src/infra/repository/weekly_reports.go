package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// WeeklyReportRepository answers the main-DB questions the weekly report asks
// about one workspace's week, and holds the per-membership opt-out. Every read
// that takes a tenantID filters on it explicitly, because the dispatcher runs
// in a system context.
type WeeklyReportRepository interface {
	// ListActiveTenants returns every active tenant, oldest first.
	ListActiveTenants(ctx context.Context) ([]ReportTenant, error)
	// TenantName returns an active tenant's name. sql.ErrNoRows when the
	// tenant is gone or not active.
	TenantName(ctx context.Context, tenantID string) (string, error)

	// ListRecipients returns the tenant's owners who haven't opted out,
	// oldest first.
	ListRecipients(ctx context.Context, tenantID string) ([]models.User, error)
	// OptedOut reports a membership's opt-out flag.
	OptedOut(ctx context.Context, userID string) (bool, error)
	// SetOptOut sets a membership's opt-out flag. sql.ErrNoRows when the
	// membership doesn't exist.
	SetOptOut(ctx context.Context, userID string, optOut bool) error

	// Counts returns the week's tallies.
	Counts(ctx context.Context, tenantID string, w ReportWindow) (WeeklyCounts, error)
	// PublishedPosts lists the posts published through Zernio in [from, to).
	PublishedPosts(ctx context.Context, tenantID string, from, to time.Time) ([]ReportPost, error)
	// Failures lists the posts whose latest transition in [from, to) was to
	// failed or not_published, newest first, capped at limit.
	Failures(ctx context.Context, tenantID string, from, to time.Time, limit int) ([]ReportFailure, error)
	// AttentionAccounts lists connected accounts that need reconnecting or
	// whose token expires before expiringBefore.
	AttentionAccounts(ctx context.Context, tenantID string, expiringBefore time.Time) ([]ReportAccount, error)
	// GoalCampaigns lists the live campaigns that have a post goal.
	GoalCampaigns(ctx context.Context, tenantID string) ([]ReportCampaign, error)
}

// ReportTenant is a workspace the dispatcher may report on.
type ReportTenant struct {
	ID   string `bun:"id"`
	Name string `bun:"name"`
}

// ReportWindow is the reported week [Start, End) plus the week before it,
// [PrevStart, Start). Now is the dispatch time.
type ReportWindow struct {
	PrevStart time.Time
	Start     time.Time
	End       time.Time
	Now       time.Time
}

// WeeklyCounts are the scalar tallies of one week.
type WeeklyCounts struct {
	PrevPublished    int `bun:"prev_published"`
	Failed           int `bun:"failed"`
	PrevFailed       int `bun:"prev_failed"`
	ScheduledNext    int `bun:"scheduled_next"`
	ManualDue        int `bun:"manual_due"`
	PostsCreated     int `bun:"posts_created"`
	CampaignsCreated int `bun:"campaigns_created"`
	IdeasWaiting     int `bun:"ideas_waiting"`
	IdeasCreated     int `bun:"ideas_created"`
	RemindersDue     int `bun:"reminders_due"`
	AssetsCreated    int `bun:"assets_created"`
	MembersTotal     int `bun:"members_total"`
	MembersJoined    int `bun:"members_joined"`
	InvitesPending   int `bun:"invites_pending"`
}

// ReportPost is a published post with its channel.
type ReportPost struct {
	ID              string    `bun:"id"`
	CampaignID      string    `bun:"campaign_id"`
	Title           string    `bun:"title"`
	Content         string    `bun:"content"`
	Platform        string    `bun:"platform"`
	SocialAccountID string    `bun:"social_account_id"`
	AccountName     string    `bun:"account_name"`
	PublishedAt     time.Time `bun:"published_at"`
}

// ReportFailure is a post that didn't go out.
type ReportFailure struct {
	PostID          string    `bun:"post_id"`
	CampaignID      string    `bun:"campaign_id"`
	Title           string    `bun:"title"`
	Content         string    `bun:"content"`
	Platform        string    `bun:"platform"`
	SocialAccountID string    `bun:"social_account_id"`
	AccountName     string    `bun:"account_name"`
	Reason          string    `bun:"failure_reason"`
	At              time.Time `bun:"event_timestamp"`
}

// ReportAccount is a connected account that needs the owner.
type ReportAccount struct {
	ID             string     `bun:"id"`
	Platform       string     `bun:"platform"`
	Name           string     `bun:"name"`
	NeedsReconnect bool       `bun:"needs_reconnect"`
	TokenExpiresAt *time.Time `bun:"token_expires_at"`
	Scheduled      int        `bun:"scheduled"`
}

// ReportCampaign is a live campaign with a post goal.
type ReportCampaign struct {
	ID   string `bun:"id"`
	Name string `bun:"name"`
}

type weeklyReportRepository struct {
	db *bun.DB
}

// NewWeeklyReportRepository builds a WeeklyReportRepository over the main DB.
func NewWeeklyReportRepository(db *bun.DB) WeeklyReportRepository {
	return &weeklyReportRepository{db: db}
}

func (r *weeklyReportRepository) ListActiveTenants(ctx context.Context) ([]ReportTenant, error) {
	var out []ReportTenant
	err := r.db.NewSelect().Table("tenants").Column("id", "name").
		Where("status = ?", models.TenantStatusActive).
		Where("deleted_at IS NULL").
		OrderExpr("created_at ASC, id ASC").
		Scan(ctx, &out)
	return out, err
}

func (r *weeklyReportRepository) TenantName(ctx context.Context, tenantID string) (string, error) {
	var name string
	err := r.db.NewSelect().Table("tenants").Column("name").
		Where("id = ?", tenantID).
		Where("status = ?", models.TenantStatusActive).
		Where("deleted_at IS NULL").
		Scan(ctx, &name)
	return name, err
}

func (r *weeklyReportRepository) ListRecipients(ctx context.Context, tenantID string) ([]models.User, error) {
	// User is not TenantScoped, so the tenant filter is explicit.
	var owners []models.User
	err := r.db.NewSelect().Model(&owners).
		Where("u.tenant_id = ?", tenantID).
		Where("u.role = ?", models.RoleOwner).
		Where("NOT u.weekly_report_opt_out").
		OrderExpr("u.created_at ASC, u.id ASC").
		Scan(ctx)
	return owners, err
}

func (r *weeklyReportRepository) OptedOut(ctx context.Context, userID string) (bool, error) {
	var out bool
	err := r.db.NewSelect().Table("users").Column("weekly_report_opt_out").
		Where("id = ?", userID).Scan(ctx, &out)
	return out, err
}

func (r *weeklyReportRepository) SetOptOut(ctx context.Context, userID string, optOut bool) error {
	res, err := r.db.NewUpdate().Table("users").
		Set("weekly_report_opt_out = ?", optOut).
		Where("id = ?", userID).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// weeklyCountsSQL tallies one week in a single round trip. Placeholders:
// ?0 tenant, ?1 prev start, ?2 start, ?3 end, ?4 end + 7 days, ?5 end + 1 day,
// ?6 now.
const weeklyCountsSQL = `
SELECT
  (SELECT count(*) FROM posts WHERE tenant_id = ?0 AND publisher = 'zernio'
     AND published_at >= ?1 AND published_at < ?2) AS prev_published,
  (SELECT count(DISTINCT pl.post_id) FROM post_logs pl JOIN posts po ON po.id = pl.post_id
     WHERE pl.tenant_id = ?0 AND pl.event_type = 'state_transition'
       AND pl.to_status IN ('failed', 'not_published')
       AND pl.event_timestamp >= ?2 AND pl.event_timestamp < ?3) AS failed,
  (SELECT count(DISTINCT pl.post_id) FROM post_logs pl JOIN posts po ON po.id = pl.post_id
     WHERE pl.tenant_id = ?0 AND pl.event_type = 'state_transition'
       AND pl.to_status IN ('failed', 'not_published')
       AND pl.event_timestamp >= ?1 AND pl.event_timestamp < ?2) AS prev_failed,
  (SELECT count(*) FROM posts WHERE tenant_id = ?0 AND status = 'scheduled'
     AND scheduled_at >= ?3 AND scheduled_at < ?4) AS scheduled_next,
  (SELECT count(*) FROM posts WHERE tenant_id = ?0 AND status = 'scheduled_for_manual_publishing'
     AND scheduled_at < ?4) AS manual_due,
  (SELECT count(*) FROM posts WHERE tenant_id = ?0
     AND created_at >= ?2 AND created_at < ?3) AS posts_created,
  (SELECT count(*) FROM campaigns WHERE tenant_id = ?0 AND deleted_at IS NULL
     AND created_at >= ?2 AND created_at < ?3) AS campaigns_created,
  (SELECT count(*) FROM ideas WHERE tenant_id = ?0 AND verdict IS NULL) AS ideas_waiting,
  (SELECT count(*) FROM ideas WHERE tenant_id = ?0
     AND created_at >= ?2 AND created_at < ?3) AS ideas_created,
  (SELECT count(*) FROM ideas WHERE tenant_id = ?0 AND verdict = 'later'
     AND remind_at < ?5) AS reminders_due,
  (SELECT count(*) FROM assets WHERE tenant_id = ?0
     AND created_at >= ?2 AND created_at < ?3) AS assets_created,
  (SELECT count(*) FROM users WHERE tenant_id = ?0) AS members_total,
  (SELECT count(*) FROM users WHERE tenant_id = ?0
     AND created_at >= ?2 AND created_at < ?3) AS members_joined,
  (SELECT count(*) FROM users_invitations WHERE tenant_id = ?0 AND status = 'pending'
     AND expires_at > ?6) AS invites_pending`

func (r *weeklyReportRepository) Counts(ctx context.Context, tenantID string, w ReportWindow) (WeeklyCounts, error) {
	var c WeeklyCounts
	err := r.db.NewRaw(weeklyCountsSQL,
		tenantID, w.PrevStart, w.Start, w.End, w.End.AddDate(0, 0, 7), w.End.AddDate(0, 0, 1), w.Now,
	).Scan(ctx, &c)
	return c, err
}

func (r *weeklyReportRepository) PublishedPosts(ctx context.Context, tenantID string, from, to time.Time) ([]ReportPost, error) {
	var out []ReportPost
	err := r.db.NewRaw(`
SELECT po.id, COALESCE(po.campaign_id, '') AS campaign_id, COALESCE(po.title, '') AS title,
       left(po.content, 200) AS content, COALESCE(pl.name, '') AS platform,
       COALESCE(po.social_account_id, '') AS social_account_id,
       COALESCE(NULLIF(sa.display_name, ''), sa.username, '') AS account_name,
       po.published_at
FROM posts po
LEFT JOIN platforms pl ON pl.id = po.platform_id
LEFT JOIN social_accounts sa ON sa.id = po.social_account_id
WHERE po.tenant_id = ? AND po.publisher = 'zernio'
  AND po.published_at >= ? AND po.published_at < ?
ORDER BY po.published_at ASC, po.id ASC`, tenantID, from, to).Scan(ctx, &out)
	return out, err
}

func (r *weeklyReportRepository) Failures(ctx context.Context, tenantID string, from, to time.Time, limit int) ([]ReportFailure, error) {
	var out []ReportFailure
	err := r.db.NewRaw(`
SELECT * FROM (
  SELECT DISTINCT ON (pl.post_id)
         pl.post_id, COALESCE(po.campaign_id, '') AS campaign_id, COALESCE(po.title, '') AS title,
         left(po.content, 200) AS content, COALESCE(pf.name, '') AS platform,
         COALESCE(po.social_account_id, '') AS social_account_id,
         COALESCE(NULLIF(sa.display_name, ''), sa.username, '') AS account_name,
         po.failure_reason, pl.event_timestamp
  FROM post_logs pl
  JOIN posts po ON po.id = pl.post_id
  LEFT JOIN platforms pf ON pf.id = po.platform_id
  LEFT JOIN social_accounts sa ON sa.id = po.social_account_id
  WHERE pl.tenant_id = ? AND pl.event_type = 'state_transition'
    AND pl.to_status IN ('failed', 'not_published')
    AND pl.event_timestamp >= ? AND pl.event_timestamp < ?
  ORDER BY pl.post_id, pl.event_timestamp DESC
) f
ORDER BY f.event_timestamp DESC
LIMIT ?`, tenantID, from, to, limit).Scan(ctx, &out)
	return out, err
}

func (r *weeklyReportRepository) AttentionAccounts(ctx context.Context, tenantID string, expiringBefore time.Time) ([]ReportAccount, error) {
	var out []ReportAccount
	err := r.db.NewRaw(`
SELECT sa.id, sa.platform,
       COALESCE(NULLIF(sa.display_name, ''), sa.username) AS name,
       COALESCE(sa.needs_reconnect, false) OR COALESCE(sa.health_status, '') = 'error'
         OR COALESCE(sa.token_valid, true) = false AS needs_reconnect,
       sa.token_expires_at,
       (SELECT count(*) FROM posts po WHERE po.social_account_id = sa.id AND po.status = 'scheduled') AS scheduled
FROM social_accounts sa
WHERE sa.tenant_id = ? AND sa.deleted_at IS NULL AND sa.is_active
  AND (COALESCE(sa.needs_reconnect, false) OR COALESCE(sa.health_status, '') = 'error'
       OR COALESCE(sa.token_valid, true) = false OR sa.token_expires_at < ?)
ORDER BY sa.platform, sa.id`, tenantID, expiringBefore).Scan(ctx, &out)
	return out, err
}

func (r *weeklyReportRepository) GoalCampaigns(ctx context.Context, tenantID string) ([]ReportCampaign, error) {
	var out []ReportCampaign
	err := r.db.NewRaw(`
SELECT id, name FROM campaigns
WHERE tenant_id = ? AND deleted_at IS NULL AND archived_at IS NULL
  AND status IN (?, ?) AND COALESCE(estimated_post_count, 0) > 0
ORDER BY start_date NULLS LAST, created_at, id`,
		tenantID, models.StatusActive, models.StatusScheduled).Scan(ctx, &out)
	return out, err
}
