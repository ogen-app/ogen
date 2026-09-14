package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// CampaignRepository defines all persistence operations for the Campaign domain.
//
// Lifecycle (CON-156 BE 6): List and GetByID exclude soft-deleted campaigns;
// List additionally excludes archived ones (reachable via ListArchived). Delete
// soft-deletes; Archive/Unarchive toggle archived_at. All are tenant-scoped by
// the model's Before* hooks.
type CampaignRepository interface {
	List(ctx context.Context) ([]models.Campaign, error)
	ListArchived(ctx context.Context) ([]models.Campaign, error)
	Create(ctx context.Context, campaign *models.Campaign) error
	GetByID(ctx context.Context, id string) (*models.Campaign, error)
	// Update writes the whole record; excludeColumns drops the named columns
	// from the write so a PUT that omits the presence-aware content-bank fields
	// (asset_ids / use_assets) doesn't clobber a concurrent membership write.
	Update(ctx context.Context, campaign *models.Campaign, excludeColumns ...string) error
	Delete(ctx context.Context, id string) (bool, error)
	Archive(ctx context.Context, id string) (bool, error)
	Unarchive(ctx context.Context, id string) (bool, error)
	// AddAssetIDs / RemoveAssetID are the CON-233 membership write path: they
	// mutate campaigns.asset_ids (and keep use_assets derived from it), in one
	// atomic UPDATE, so attaching or detaching one document no longer round-trips
	// the whole record and concurrent adds of different ids don't clobber each
	// other. Both return the hydrated campaign, or sql.ErrNoRows if it doesn't
	// exist (in this tenant).
	AddAssetIDs(ctx context.Context, id string, assetIDs []string) (*models.Campaign, error)
	RemoveAssetID(ctx context.Context, id, assetID string) (*models.Campaign, error)
	// CountActive counts the tenant's live campaigns — neither soft-deleted nor
	// archived (mirrors List) — the active_campaigns quota (CON-295).
	CountActive(ctx context.Context) (int64, error)
}

type campaignRepository struct {
	db               *bun.DB
	tagRepo          TagRepository
	platformRepo     PlatformRepository
	campaignTypeRepo CampaignTypeRepository
}

// NewCampaignRepository returns a Bun-backed CampaignRepository.
func NewCampaignRepository(
	db *bun.DB,
	tagRepo TagRepository,
	platformRepo PlatformRepository,
	campaignTypeRepo CampaignTypeRepository,
) CampaignRepository {
	return &campaignRepository{
		db:               db,
		tagRepo:          tagRepo,
		platformRepo:     platformRepo,
		campaignTypeRepo: campaignTypeRepo,
	}
}

// List returns the active set: campaigns that are neither soft-deleted nor
// archived, oldest first.
func (r *campaignRepository) List(ctx context.Context) ([]models.Campaign, error) {
	return r.listFiltered(ctx, func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Where("c.archived_at IS NULL")
	})
}

func (r *campaignRepository) CountActive(ctx context.Context) (int64, error) {
	n, err := r.db.NewSelect().Model((*models.Campaign)(nil)).
		Where("c.deleted_at IS NULL").
		Where("c.archived_at IS NULL").
		Count(ctx)
	return int64(n), err
}

// ListArchived returns archived (but not soft-deleted) campaigns, oldest first.
func (r *campaignRepository) ListArchived(ctx context.Context) ([]models.Campaign, error) {
	return r.listFiltered(ctx, func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Where("c.archived_at IS NOT NULL")
	})
}

// listFiltered runs the shared list query + hydration, always excluding
// soft-deleted rows and applying the caller's archived-state predicate.
func (r *campaignRepository) listFiltered(ctx context.Context, extra func(*bun.SelectQuery) *bun.SelectQuery) ([]models.Campaign, error) {
	var campaigns []models.Campaign
	q := extra(r.db.NewSelect().Model(&campaigns).Where("c.deleted_at IS NULL"))
	if err := q.OrderExpr("created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	if err := r.hydrateTags(ctx, campaigns); err != nil {
		return nil, err
	}
	if err := r.hydratePlatforms(ctx, campaigns); err != nil {
		return nil, err
	}
	if err := r.hydrateCampaignTypes(ctx, campaigns); err != nil {
		return nil, err
	}
	return campaigns, nil
}

func (r *campaignRepository) Create(ctx context.Context, campaign *models.Campaign) error {
	_, err := r.db.NewInsert().Model(campaign).Exec(ctx)
	return err
}

// GetByID returns a single campaign by id. Soft-deleted campaigns are treated
// as gone (sql.ErrNoRows); archived campaigns are still returned so they can be
// viewed and unarchived.
func (r *campaignRepository) GetByID(ctx context.Context, id string) (*models.Campaign, error) {
	campaign := new(models.Campaign)
	err := r.db.NewSelect().Model(campaign).Where("c.id = ?", id).Where("c.deleted_at IS NULL").Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	campaigns := []models.Campaign{*campaign}
	if err := r.hydrateTags(ctx, campaigns); err != nil {
		return nil, err
	}
	if err := r.hydratePlatforms(ctx, campaigns); err != nil {
		return nil, err
	}
	if err := r.hydrateCampaignTypes(ctx, campaigns); err != nil {
		return nil, err
	}
	return &campaigns[0], nil
}

// Update writes the whole campaign record. excludeColumns drops the named
// columns from the UPDATE: the CON-233 content-bank fields (asset_ids /
// use_assets) have their own atomic membership write path, so a whole-record
// PUT that omits them must leave the stored value untouched at the DB — not
// merely restate the hydrated (pre-read) value, which would clobber a
// concurrent AddAssetIDs/RemoveAssetID that landed after the caller's GetByID.
func (r *campaignRepository) Update(ctx context.Context, campaign *models.Campaign, excludeColumns ...string) error {
	q := r.db.NewUpdate().Model(campaign).WherePK()
	if len(excludeColumns) > 0 {
		q = q.ExcludeColumn(excludeColumns...)
	}
	_, err := q.Exec(ctx)
	return err
}

// AddAssetIDs unions assetIDs into campaigns.asset_ids atomically (CON-233).
// An empty input is a no-op read: it still validates existence and returns the
// current campaign, so the caller gets a 404 for a missing id without a
// pointless write. The TenantScoped hook scopes the UPDATE to the caller's
// tenant, so a foreign or unknown id matches zero rows and surfaces as
// sql.ErrNoRows.
func (r *campaignRepository) AddAssetIDs(ctx context.Context, id string, assetIDs []string) (*models.Campaign, error) {
	if len(assetIDs) == 0 {
		return r.GetByID(ctx, id)
	}
	payload, err := json.Marshal(assetIDs)
	if err != nil {
		return nil, err
	}
	res, err := r.db.NewUpdate().
		Model((*models.Campaign)(nil)).
		Set(jsonbIDUnionSet("asset_ids"), string(payload)).
		// CON-233: keep use_assets in lockstep with the set. The content-bank set
		// is the FE's only source of truth for this flag since CON-210 retired the
		// three-mode picker, and resolveAssets returns early when use_assets is
		// false — so without this a first attach to a brief-only campaign is
		// silently ignored by generation. A union with non-empty input is always
		// non-empty (empty input is handled by the no-op read above), so attaching
		// always turns generation on.
		Set("use_assets = ?", true).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return r.GetByID(ctx, id)
}

// RemoveAssetID drops assetID from campaigns.asset_ids atomically (CON-233).
// Removal is idempotent — an absent id still matches the row (so it is not a
// 404) and leaves the set unchanged. use_assets is re-derived from the set (see
// AddAssetIDs), so detaching the last source turns generation off rather than
// leaving use_assets=true over an empty list — which collectReadyCandidateIDs
// would read as "every asset in the workspace".
func (r *campaignRepository) RemoveAssetID(ctx context.Context, id, assetID string) (*models.Campaign, error) {
	res, err := r.db.NewUpdate().
		Model((*models.Campaign)(nil)).
		Set(jsonbIDRemoveSet("asset_ids"), assetID).
		// Every SET reads the pre-update row, so `asset_ids - ?` here is exactly the
		// value being stored by the Set above: use_assets tracks the post-removal
		// length, never the stale pre-removal one.
		Set("use_assets = (jsonb_array_length(asset_ids - ?) > 0)", assetID).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return r.GetByID(ctx, id)
}

// Delete soft-deletes a campaign by stamping deleted_at. Returns false when the
// campaign does not exist (in this tenant) or was already deleted. The row is
// retained as an operational safety net; there is no self-serve restore.
func (r *campaignRepository) Delete(ctx context.Context, id string) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.Campaign)(nil)).
		Set("deleted_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("deleted_at IS NULL").
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Archive stamps archived_at, removing the campaign from the default active
// list. Idempotent: re-archiving an already archived campaign preserves the
// original timestamp. Returns false only when the campaign is missing or
// soft-deleted.
func (r *campaignRepository) Archive(ctx context.Context, id string) (bool, error) {
	now := time.Now().UTC()
	return r.setArchivedAt(ctx, id, &now)
}

// Unarchive clears archived_at, returning the campaign to the active list.
// Idempotent. Returns false only when the campaign is missing or soft-deleted.
func (r *campaignRepository) Unarchive(ctx context.Context, id string) (bool, error) {
	return r.setArchivedAt(ctx, id, nil)
}

// setArchivedAt sets archived_at on a live (not deleted) campaign, returning
// whether a row matched. A non-nil at archives while preserving any existing
// timestamp (re-archiving must not move it); a nil at clears the field.
func (r *campaignRepository) setArchivedAt(ctx context.Context, id string, at *time.Time) (bool, error) {
	q := r.db.NewUpdate().Model((*models.Campaign)(nil)).
		Where("id = ?", id).
		Where("deleted_at IS NULL")
	if at != nil {
		// COALESCE keeps the first archive time on a re-archive while still
		// stamping it when archived_at is currently NULL.
		q = q.Set("archived_at = COALESCE(archived_at, ?)", at)
	} else {
		q = q.Set("archived_at = NULL")
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *campaignRepository) hydratePlatforms(ctx context.Context, campaigns []models.Campaign) error {
	for i := range campaigns {
		campaigns[i].Platforms = []models.Platform{}
	}
	ids := collectIDsFlat(campaigns, func(c models.Campaign) []string {
		out := make([]string, len(c.TargetPlatforms))
		for i, tp := range c.TargetPlatforms {
			out[i] = tp.ID
		}
		return out
	})
	byID, err := fetchByIDs(ctx, r.db, ids, func(p *models.Platform) string { return p.ID })
	if err != nil {
		return err
	}
	for i, c := range campaigns {
		for _, tp := range c.TargetPlatforms {
			if p, ok := byID[tp.ID]; ok {
				campaigns[i].Platforms = append(campaigns[i].Platforms, *p)
			}
		}
	}
	return nil
}

func (r *campaignRepository) hydrateCampaignTypes(ctx context.Context, campaigns []models.Campaign) error {
	ids := make([]string, 0, len(campaigns))
	for _, c := range campaigns {
		ids = append(ids, c.CampaignTypeID)
	}
	byID, err := r.campaignTypeRepo.GetByIDs(ctx, ids)
	if err != nil {
		return err
	}
	for i, c := range campaigns {
		if ct, ok := byID[c.CampaignTypeID]; ok {
			campaigns[i].CampaignType = ct
		}
	}
	return nil
}

func (r *campaignRepository) hydrateTags(ctx context.Context, campaigns []models.Campaign) error {
	return HydrateTags(ctx, r.tagRepo, campaigns,
		func(c models.Campaign) []string { return c.TagIDs },
		func(c *models.Campaign) *[]models.Tag { return &c.Tags },
	)
}
