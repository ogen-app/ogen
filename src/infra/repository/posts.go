package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PostRepository defines all persistence operations for the Post domain.
type PostRepository interface {
	List(ctx context.Context) ([]models.Post, error)
	ListByCampaign(ctx context.Context, campaignID string) ([]models.Post, error)
	Create(ctx context.Context, post *models.Post) error
	CreateBatch(ctx context.Context, posts []*models.Post) error
	GetByID(ctx context.Context, id string) (*models.Post, error)
	// Update writes the whole record; excludeColumns drops the named columns
	// from the write so a PUT that omits the presence-aware source set
	// (used_asset_ids) doesn't clobber a concurrent membership write.
	Update(ctx context.Context, post *models.Post, excludeColumns ...string) error
	// AddUsedAssetIDs / RemoveUsedAssetID are the CON-233 membership write path
	// for a post's sources: they mutate only posts.used_asset_ids, in one atomic
	// UPDATE, so attaching or detaching one source no longer round-trips the whole
	// record and concurrent adds of different ids don't clobber each other. Both
	// return the hydrated post, or sql.ErrNoRows if it doesn't exist (in this
	// tenant). The CON-251 submitted-post content-lock is enforced here too, under
	// a row lock (SELECT ... FOR UPDATE): a real source change to a submitted post
	// is refused with *PostSubmittedError even if a concurrent schedule/publish
	// wins the race against the handler's optimistic pre-check.
	AddUsedAssetIDs(ctx context.Context, id string, assetIDs []string) (*models.Post, error)
	RemoveUsedAssetID(ctx context.Context, id, assetID string) (*models.Post, error)
	// UpdateScheduledAtBatch updates only the scheduled_at (+ updated_at)
	// column of several posts atomically (CON-115 redistribution).
	UpdateScheduledAtBatch(ctx context.Context, posts []*models.Post) error
	Delete(ctx context.Context, id string) (bool, error)
	// ListScheduledByPlatform returns every post in status='scheduled'
	// for the given platform id (Sqid) — the posts that still carry a
	// live auto-publish Zernio job. CON-130 uses it to convert all of a
	// platform's upcoming auto-publish posts to manual publishing when its
	// allowlist entry is turned off. No scheduled_at filter: a scheduled
	// post that is overdue but not yet published would still auto-publish,
	// so it must be converted too.
	ListScheduledByPlatform(ctx context.Context, platformID string) ([]models.Post, error)
	// CON-69 §8: reconciliation sweeper helpers.
	ListStuckScheduled(ctx context.Context, cutoff time.Time, limit int) ([]models.Post, error)
	UpdateStatusAndReason(ctx context.Context, postID string, status models.PostStatus, reason string) error
	// CON-93 §6 FR2: build the publisher_post_id → post_id match map the
	// analytics refresh keys off. Returns id + publisher_post_id only,
	// for Zernio-published posts that actually carry a publisher post id.
	ListWithPublisherPostID(ctx context.Context) ([]models.Post, error)
	// ListSummaryProjections returns a slim projection of every post in the
	// tenant — only the columns the Campaigns-list readiness rules read
	// (CON-152), with no relation hydration. One batched read replaces the N
	// per-card GET /campaigns/:id/posts requests the list used to fire.
	ListSummaryProjections(ctx context.Context) ([]models.Post, error)
	// CountPendingByAccount returns how many of the tenant's posts still
	// reference the given social account in a not-yet-published, committed-to-
	// publish state (scheduled or scheduled_for_manual_publishing). CON-133
	// disconnect uses it to guard against stranding queued posts: a non-zero
	// count blocks the disconnect with 409 unless the caller forces it.
	CountPendingByAccount(ctx context.Context, socialAccountID string) (int, error)
	// PublishedAtsBetween returns the publish timestamps of the tenant's
	// Zernio-published posts in [from, to), ascending — the "posts published"
	// metric for the CON-237 overview (count + cumulative-by-day series). Kept on
	// the main DB (source of truth for what shipped), composed app-side with the
	// analytics-DB series.
	PublishedAtsBetween(ctx context.Context, from, to time.Time) ([]time.Time, error)
	// ListPublishedSince returns the tenant's Zernio-published posts with
	// published_at >= since (zero since = all-time), ascending, WITHOUT relation
	// hydration — the CON-239 "what works / fading" miner reads only the scalar
	// content/media/cta columns and joins metrics app-side by post id.
	ListPublishedSince(ctx context.Context, since time.Time) ([]models.Post, error)
	// PublishedProjectionBetween returns id + platform_id + published_at for the
	// tenant's posts published in [from, to) — a zero from means unbounded-low —
	// newest first, optionally narrowed to one campaign. limit 0 = no cap. Feeds
	// the Activity daily report's "published" bucket (CON-285).
	PublishedProjectionBetween(ctx context.Context, from, to time.Time, campaignID string, limit int) ([]models.Post, error)
	// CreatedProjectionBetween returns id + created_by + scheduled_at + created_at
	// for the tenant's posts created in [from, to) — a zero from means
	// unbounded-low — newest first, optionally one campaign. limit 0 = no cap.
	// Feeds the Activity report's "created" bucket (CON-285).
	CreatedProjectionBetween(ctx context.Context, from, to time.Time, campaignID string, limit int) ([]models.Post, error)
}

type postRepository struct {
	db *bun.DB
}

// NewPostRepository returns a Bun-backed PostRepository.
func NewPostRepository(db *bun.DB) PostRepository {
	return &postRepository{db: db}
}

func (r *postRepository) ListPublishedSince(ctx context.Context, since time.Time) ([]models.Post, error) {
	var posts []models.Post
	q := r.db.NewSelect().Model(&posts).
		Where("po.publisher = ?", "zernio").
		Where("po.published_at IS NOT NULL").
		OrderExpr("po.published_at ASC")
	if !since.IsZero() {
		q = q.Where("po.published_at >= ?", since)
	}
	// No hydrateRelations: the miner reads only scalar columns (content,
	// media_urls, platform_post_type, cta_*, published_at).
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return posts, nil
}

func (r *postRepository) PublishedAtsBetween(ctx context.Context, from, to time.Time) ([]time.Time, error) {
	var ats []time.Time
	// BeforeSelect adds the tenant predicate. published_at IS NOT NULL is implied
	// by the range bounds (NULL compares false), but stated for clarity.
	if err := r.db.NewSelect().Model((*models.Post)(nil)).
		Column("published_at").
		Where("po.publisher = ?", "zernio").
		Where("po.published_at >= ?", from).
		Where("po.published_at < ?", to).
		OrderExpr("po.published_at ASC").
		Scan(ctx, &ats); err != nil {
		return nil, err
	}
	return ats, nil
}

func (r *postRepository) PublishedProjectionBetween(ctx context.Context, from, to time.Time, campaignID string, limit int) ([]models.Post, error) {
	var posts []models.Post
	q := r.db.NewSelect().Model(&posts).
		Column("id", "platform_id", "published_at").
		Where("po.published_at IS NOT NULL").
		Where("po.published_at < ?", to)
	if !from.IsZero() {
		q = q.Where("po.published_at >= ?", from)
	}
	if campaignID != "" {
		q = q.Where("po.campaign_id = ?", campaignID)
	}
	q = q.OrderExpr("po.published_at DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return posts, nil
}

func (r *postRepository) CreatedProjectionBetween(ctx context.Context, from, to time.Time, campaignID string, limit int) ([]models.Post, error) {
	var posts []models.Post
	q := r.db.NewSelect().Model(&posts).
		Column("id", "created_by", "scheduled_at", "created_at").
		Where("po.created_at < ?", to)
	if !from.IsZero() {
		q = q.Where("po.created_at >= ?", from)
	}
	if campaignID != "" {
		q = q.Where("po.campaign_id = ?", campaignID)
	}
	q = q.OrderExpr("po.created_at DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return posts, nil
}

func (r *postRepository) List(ctx context.Context) ([]models.Post, error) {
	var posts []models.Post
	if err := r.db.NewSelect().Model(&posts).OrderExpr("created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	if err := r.hydrateRelations(ctx, posts); err != nil {
		return nil, err
	}
	return posts, nil
}

func (r *postRepository) ListByCampaign(ctx context.Context, campaignID string) ([]models.Post, error) {
	var posts []models.Post
	if err := r.db.NewSelect().Model(&posts).Where("po.campaign_id = ?", campaignID).OrderExpr("created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	if err := r.hydrateRelations(ctx, posts); err != nil {
		return nil, err
	}
	return posts, nil
}

func (r *postRepository) Create(ctx context.Context, post *models.Post) error {
	_, err := r.db.NewInsert().Model(post).Exec(ctx)
	return err
}

func (r *postRepository) CreateBatch(ctx context.Context, posts []*models.Post) error {
	if len(posts) == 0 {
		return nil
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.NewInsert().Model(&posts).Exec(ctx)
		return err
	})
}

func (r *postRepository) GetByID(ctx context.Context, id string) (*models.Post, error) {
	post := new(models.Post)
	err := r.db.NewSelect().Model(post).Where("po.id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	posts := []models.Post{*post}
	if err := r.hydrateRelations(ctx, posts); err != nil {
		return nil, err
	}
	return &posts[0], nil
}

// Update writes the whole post record. excludeColumns drops the named columns
// from the UPDATE: the CON-233 source set (used_asset_ids) has its own atomic
// membership write path, so a whole-record PUT that omits it must leave the
// stored value untouched at the DB — not restate the hydrated (pre-read) value,
// which would clobber a concurrent AddUsedAssetIDs/RemoveUsedAssetID that landed
// after the caller's GetByID.
func (r *postRepository) Update(ctx context.Context, post *models.Post, excludeColumns ...string) error {
	q := r.db.NewUpdate().Model(post).WherePK()
	if len(excludeColumns) > 0 {
		q = q.ExcludeColumn(excludeColumns...)
	}
	_, err := q.Exec(ctx)
	return err
}

// PostSubmittedError reports that a CON-233 source-membership write was refused
// because the post is in a submitted state (scheduled/published) and its sources
// are frozen (CON-251). The membership repo methods verify the status and apply
// the write atomically under a row lock, so a concurrent schedule/publish that
// slips in after a handler's optimistic pre-check is still caught here and
// surfaced as a lock (HTTP 409) rather than silently mutating a frozen post. The
// carried Status feeds the handler's 409 message.
type PostSubmittedError struct {
	Status models.PostStatus
}

func (e *PostSubmittedError) Error() string {
	return "post is submitted (" + string(e.Status) + "); its sources are locked"
}

// AddUsedAssetIDs unions assetIDs into posts.used_asset_ids atomically (CON-233),
// mirroring campaignRepository.AddAssetIDs. An empty input is a no-op read that
// still validates existence. The TenantScoped hook scopes the UPDATE, so an
// unknown or foreign id surfaces as sql.ErrNoRows. A real change to a submitted
// post is refused with *PostSubmittedError (CON-251); see mutateUsedAssets.
func (r *postRepository) AddUsedAssetIDs(ctx context.Context, id string, assetIDs []string) (*models.Post, error) {
	if len(assetIDs) == 0 {
		return r.GetByID(ctx, id)
	}
	payload, err := json.Marshal(assetIDs)
	if err != nil {
		return nil, err
	}
	// A union adds nothing (no real change) only when every incoming id is
	// already present.
	changes := func(current models.StringSlice) bool {
		for _, a := range assetIDs {
			if !slices.Contains(current, a) {
				return true
			}
		}
		return false
	}
	if err := r.mutateUsedAssets(ctx, id, jsonbIDUnionSet("used_asset_ids"), string(payload), changes); err != nil {
		return nil, err
	}
	return r.GetByID(ctx, id)
}

// RemoveUsedAssetID drops assetID from posts.used_asset_ids atomically (CON-233).
// Removal is idempotent — an absent id still matches the row and changes nothing.
// A real change to a submitted post is refused with *PostSubmittedError
// (CON-251); see mutateUsedAssets.
func (r *postRepository) RemoveUsedAssetID(ctx context.Context, id, assetID string) (*models.Post, error) {
	changes := func(current models.StringSlice) bool {
		return slices.Contains(current, assetID)
	}
	if err := r.mutateUsedAssets(ctx, id, jsonbIDRemoveSet("used_asset_ids"), assetID, changes); err != nil {
		return nil, err
	}
	return r.GetByID(ctx, id)
}

// mutateUsedAssets applies one used_asset_ids write (setExpr/arg) atomically with
// the CON-251 content-lock. It locks the row FOR UPDATE, so the submitted-state
// check and the write cannot straddle a concurrent schedule/publish: if the
// change would really alter the set (changes reports true) on a submitted post,
// it refuses with *PostSubmittedError instead of writing. A no-op is allowed on
// any status (matching the handler's no-op rule); an unknown or foreign id
// surfaces as sql.ErrNoRows.
func (r *postRepository) mutateUsedAssets(ctx context.Context, id, setExpr string, arg any, changes func(current models.StringSlice) bool) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		current := new(models.Post)
		err := tx.NewSelect().Model(current).
			Column("id", "status", "used_asset_ids").
			Where("po.id = ?", id).
			For("UPDATE").
			Scan(ctx)
		if err != nil {
			return err // sql.ErrNoRows when missing or in another tenant
		}
		if changes(current.UsedAssetIDs) && current.Status.IsSubmitted() {
			return &PostSubmittedError{Status: current.Status}
		}
		res, err := tx.NewUpdate().
			Model((*models.Post)(nil)).
			Set(setExpr, arg).
			Set("updated_at = ?", time.Now().UTC()).
			Where("po.id = ?", id).
			Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

func (r *postRepository) CountPendingByAccount(ctx context.Context, socialAccountID string) (int, error) {
	// BeforeSelect adds the tenant predicate; the status filter is the set of
	// states that would still attempt to publish against this account. draft /
	// ready_for_publish are excluded — they're freely editable, not committed.
	return r.db.NewSelect().Model((*models.Post)(nil)).
		Where("po.social_account_id = ?", socialAccountID).
		Where("po.status IN (?)", bun.In([]models.PostStatus{
			models.PostStatusScheduled,
			models.PostStatusScheduledForManualPublish,
		})).
		Count(ctx)
}

func (r *postRepository) UpdateScheduledAtBatch(ctx context.Context, posts []*models.Post) error {
	if len(posts) == 0 {
		return nil
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, p := range posts {
			// Only the schedule columns are written; the TenantScoped hook adds
			// the tenant predicate, WherePK adds the id. The status guard limits
			// the write to still-eligible posts, so a post that was concurrently
			// published, scheduled, or already redistributed matches zero rows,
			// and the exactly-one-row check fails the whole transaction rather
			// than clobbering a schedule that moved out from under us.
			res, err := tx.NewUpdate().Model(p).
				Column("scheduled_at", "updated_at").
				Where("status IN (?)", bun.In([]models.PostStatus{
					models.PostStatusDraft, models.PostStatusReadyForPublish,
				})).
				WherePK().
				Exec(ctx)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return fmt.Errorf("post %s is no longer eligible for redistribution (status changed or removed)", p.ID)
			}
		}
		return nil
	})
}

func (r *postRepository) Delete(ctx context.Context, id string) (bool, error) {
	res, err := r.db.NewDelete().Model((*models.Post)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListStuckScheduled returns Posts in status='scheduled' whose
// scheduled_at is older than cutoff, capped at limit. Used by the
// reconciliation sweeper (CON-69 §8). NULL scheduled_at rows are
// skipped — a Scheduled post without a scheduled_at is a data
// integrity bug, not a reconciliation candidate.
func (r *postRepository) ListStuckScheduled(ctx context.Context, cutoff time.Time, limit int) ([]models.Post, error) {
	if limit <= 0 {
		limit = 100
	}
	var posts []models.Post
	err := r.db.NewSelect().
		Model(&posts).
		// Only sweep posts owned by active tenants (CON-190): a suspended or
		// deleted tenant's scheduled posts are left untouched, not forced Failed.
		Join("JOIN tenants AS t ON t.id = po.tenant_id").
		Where("t.status = ?", models.TenantStatusActive).
		Where("po.status = ?", models.PostStatusScheduled).
		Where("po.scheduled_at IS NOT NULL").
		Where("po.scheduled_at < ?", cutoff).
		OrderExpr("po.scheduled_at ASC").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return posts, nil
}

// ListScheduledByPlatform returns the tenant's posts in
// status='scheduled' for platformID (a platform Sqid), oldest
// scheduled_at first. See the interface doc for why scheduled_at is not
// filtered (CON-130).
func (r *postRepository) ListScheduledByPlatform(ctx context.Context, platformID string) ([]models.Post, error) {
	var posts []models.Post
	err := r.db.NewSelect().
		Model(&posts).
		Where("po.status = ?", models.PostStatusScheduled).
		Where("po.platform_id = ?", platformID).
		OrderExpr("po.scheduled_at ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return posts, nil
}

// UpdateStatusAndReason atomically sets status + failure_reason on
// one Post. Used by the reconciliation sweeper to avoid loading the
// full row just to flip two fields.
func (r *postRepository) UpdateStatusAndReason(ctx context.Context, postID string, status models.PostStatus, reason string) error {
	_, err := r.db.NewUpdate().
		Model((*models.Post)(nil)).
		Set("status = ?", status).
		Set("failure_reason = ?", reason).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", postID).
		Exec(ctx)
	return err
}

// ListWithPublisherPostID returns the (id, publisher_post_id) projection
// for every post that carries the Zernio publisher marker and a non-empty
// publisher_post_id — i.e. that was submitted through Zernio, whatever its
// current Ogen status. Status is deliberately NOT filtered: a partial
// publish maps to Ogen `failed` (see poll_zernio_status) yet still has real
// per-platform analytics that CON-93 §10 requires us to surface, and
// Zernio's own source=late filter is what gates which posts actually have
// analytics. The analytics refresh queue turns this into a
// publisher_post_id → post_id map to match the batch Zernio returns back to
// local posts (CON-93 §6 FR2). No relation hydration — only the two columns
// are needed.
func (r *postRepository) ListWithPublisherPostID(ctx context.Context) ([]models.Post, error) {
	var posts []models.Post
	err := r.db.NewSelect().
		Model(&posts).
		// title / published_at / platform_id / publisher are denormalised onto
		// the analytics snapshot at refresh time (CON-125 Track B), so the
		// overview read needs no cross-DB join back to posts/platforms.
		Column("id", "tenant_id", "publisher_post_id", "title", "published_at", "platform_id", "publisher").
		// Only refresh analytics for active tenants (CON-190): a suspended or
		// deleted tenant's posts are excluded from the cross-tenant sweep.
		Join("JOIN tenants AS t ON t.id = po.tenant_id").
		Where("t.status = ?", models.TenantStatusActive).
		Where("po.publisher = ?", models.PublisherZernio).
		Where("po.publisher_post_id IS NOT NULL").
		Where("po.publisher_post_id <> ''").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return posts, nil
}

// ListSummaryProjections returns a lightweight projection of every post in the
// caller's tenant — only the columns lib/campaignReadiness reads to score a
// campaign on the Campaigns list (CON-152): status + schedule/publish times +
// platform, post-type, and phase ids + media presence. It deliberately skips
// relation hydration and the heavy title/content columns, so one batched read
// replaces the N per-card GET /campaigns/:id/posts requests without shipping
// Markdown bodies or hydrated campaign/platform/asset graphs.
//
// Rows are ordered by campaign so the caller groups them in a single pass.
// Tenant scoping is applied by the TenantScoped BeforeSelect hook on
// models.Post — hence the real model rather than a bespoke struct, which would
// bypass the hook and leak across tenants.
func (r *postRepository) ListSummaryProjections(ctx context.Context) ([]models.Post, error) {
	var posts []models.Post
	err := r.db.NewSelect().
		Model(&posts).
		Column("id", "campaign_id", "status", "scheduled_at", "published_at",
			"platform_id", "platform_post_type", "campaign_type_phase_id",
			"media_urls", "created_by", "failure_reason", "created_at", "updated_at").
		OrderExpr("campaign_id ASC, created_at ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return posts, nil
}

func (r *postRepository) hydrateRelations(ctx context.Context, posts []models.Post) error {
	for i := range posts {
		posts[i].UsedAssets = []models.Asset{}
	}

	campaignIDs := collectIDs(posts, func(p models.Post) string { return p.CampaignID })
	platformIDs := collectIDs(posts, func(p models.Post) string { return p.PlatformID })
	socialAccountIDs := collectIDs(posts, func(p models.Post) string { return p.SocialAccountID })
	assetIDs := collectIDsFlat(posts, func(p models.Post) []string { return p.UsedAssetIDs })
	phaseIDs := collectIDsPtr(posts, func(p models.Post) *string { return p.CampaignTypePhaseID })

	campaignByID, err := fetchByIDs[models.Campaign](ctx, r.db, campaignIDs, func(c *models.Campaign) string { return c.ID })
	if err != nil {
		return err
	}
	for _, c := range campaignByID {
		c.Tags = []models.Tag{}
	}

	platformByID, err := fetchByIDs[models.Platform](ctx, r.db, platformIDs, func(p *models.Platform) string { return p.ID })
	if err != nil {
		return err
	}

	// CON-150: hydrate the chosen same-platform account. Soft-deleted
	// (disconnected) rows are still fetched so the UI can show which
	// account a historical post targeted.
	socialAccountByID, err := fetchByIDs[models.SocialAccount](ctx, r.db, socialAccountIDs, func(a *models.SocialAccount) string { return a.ID })
	if err != nil {
		return err
	}

	assetByID, err := fetchByIDs[models.Asset](ctx, r.db, assetIDs, func(p *models.Asset) string { return p.ID })
	if err != nil {
		return err
	}
	for _, p := range assetByID {
		p.Tags = []models.Tag{}
	}

	phaseByID, err := fetchByIDs[models.CampaignTypePhase](ctx, r.db, phaseIDs, func(p *models.CampaignTypePhase) string { return p.ID })
	if err != nil {
		return err
	}

	for i, p := range posts {
		posts[i].Campaign = campaignByID[p.CampaignID]
		posts[i].Platform = platformByID[p.PlatformID]
		posts[i].SocialAccount = socialAccountByID[p.SocialAccountID]
		for _, id := range p.UsedAssetIDs {
			if asset, ok := assetByID[id]; ok {
				posts[i].UsedAssets = append(posts[i].UsedAssets, *asset)
			}
		}
		if p.CampaignTypePhaseID != nil {
			posts[i].CampaignTypePhase = phaseByID[*p.CampaignTypePhaseID]
		}
	}
	return nil
}
