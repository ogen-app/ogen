package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// AssetRepository defines all persistence operations for the Asset domain.
type AssetRepository interface {
	List(ctx context.Context) ([]models.Asset, error)
	// Count counts the tenant's content-bank assets — the content_bank_assets
	// quota (CON-295).
	Count(ctx context.Context) (int64, error)
	Create(ctx context.Context, asset *models.Asset) error
	GetByID(ctx context.Context, id string) (*models.Asset, error)
	// GetBySourceURL returns the caller-tenant's URL asset with this source_url,
	// or sql.ErrNoRows when none exists — the dedupe lookup behind create-or-
	// refresh (CON-222). Tenant scoping comes from the TenantScoped hooks.
	GetBySourceURL(ctx context.Context, sourceURL string) (*models.Asset, error)
	Update(ctx context.Context, asset *models.Asset) error
	UpdateStatus(ctx context.Context, id, status string) error
	// CreatorOf returns an asset's created_by (its owner), tenant-scoped — used
	// to address ingest-completion notifications (CON-242). sql.ErrNoRows when
	// the asset is gone.
	CreatorOf(ctx context.Context, id string) (string, error)
	// UpdateContent sets title + content (and bumps updated_at) without touching
	// status/source_url — the process_url worker's write after a scrape (CON-222).
	UpdateContent(ctx context.Context, id, title, content string) error
	// SetImageResult writes the vision description (content) of an IMG asset and,
	// when setAlt is true, its generated alt text — but the alt write is guarded in
	// SQL so it NEVER overwrites a user's edit (CON-281 D5): alt_text is set only
	// where alt_text_edited_by_user is false. content is always written. Title is
	// left untouched (the upload filename stays the title).
	SetImageResult(ctx context.Context, id, content, altText string, setAlt bool) error
	// SetAltText sets an image asset's alt text unconditionally (CON-281) — the
	// explicit "regenerate alt text" action, which overwrites even a prior
	// generated value. It does NOT flip alt_text_edited_by_user (a regeneration is
	// not a manual edit); only a user PUT marks the text hand-edited.
	SetAltText(ctx context.Context, id, altText string) error
	// ApplyTags adds and removes tag IDs across many assets in one transaction —
	// the Content Bank's bulk-filing operation (CON-279). Each asset keeps its
	// existing tags minus `remove` plus `add`, deduped and order-preserving. The
	// load and the writes are tenant-scoped by the TenantScoped hooks, so asset
	// IDs outside the caller's tenant are silently skipped. Every `add` id must
	// resolve to a tag in the caller's tenant or the call fails with
	// ErrUnknownTag; `remove` ids are applied as-is. Returns the updated assets
	// (never nil) with tags and files hydrated.
	ApplyTags(ctx context.Context, assetIDs, add, remove []string) ([]models.Asset, error)
	Delete(ctx context.Context, id string) (bool, error)
}

type assetRepository struct {
	db       *bun.DB
	tagRepo  TagRepository
	fileRepo AssetFileRepository
}

// NewAssetRepository returns a Bun-backed AssetRepository. fileRepo may be
// nil, in which case asset.File hydration is skipped.
func NewAssetRepository(db *bun.DB, tagRepo TagRepository, fileRepo AssetFileRepository) AssetRepository {
	return &assetRepository{db: db, tagRepo: tagRepo, fileRepo: fileRepo}
}

func (r *assetRepository) Count(ctx context.Context) (int64, error) {
	n, err := r.db.NewSelect().Model((*models.Asset)(nil)).Count(ctx)
	return int64(n), err
}

func (r *assetRepository) List(ctx context.Context) ([]models.Asset, error) {
	var assets []models.Asset
	if err := r.db.NewSelect().Model(&assets).OrderExpr("created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	if err := r.hydrateTags(ctx, assets); err != nil {
		return nil, err
	}
	if err := r.hydrateFiles(ctx, assets); err != nil {
		return nil, err
	}
	return assets, nil
}

func (r *assetRepository) Create(ctx context.Context, asset *models.Asset) error {
	_, err := r.db.NewInsert().Model(asset).Exec(ctx)
	return err
}

func (r *assetRepository) GetByID(ctx context.Context, id string) (*models.Asset, error) {
	asset := new(models.Asset)
	err := r.db.NewSelect().Model(asset).Where("a.id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	assets := []models.Asset{*asset}
	if err := r.hydrateTags(ctx, assets); err != nil {
		return nil, err
	}
	if err := r.hydrateFiles(ctx, assets); err != nil {
		return nil, err
	}
	return &assets[0], nil
}

func (r *assetRepository) GetBySourceURL(ctx context.Context, sourceURL string) (*models.Asset, error) {
	asset := new(models.Asset)
	err := r.db.NewSelect().
		Model(asset).
		Where("a.source_url = ?", sourceURL).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	assets := []models.Asset{*asset}
	if err := r.hydrateTags(ctx, assets); err != nil {
		return nil, err
	}
	if err := r.hydrateFiles(ctx, assets); err != nil {
		return nil, err
	}
	return &assets[0], nil
}

func (r *assetRepository) Update(ctx context.Context, asset *models.Asset) error {
	_, err := r.db.NewUpdate().Model(asset).WherePK().Exec(ctx)
	return err
}

func (r *assetRepository) UpdateStatus(ctx context.Context, id, status string) error {
	_, err := r.db.NewUpdate().
		Model((*models.Asset)(nil)).
		Set("status = ?", status).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

func (r *assetRepository) CreatorOf(ctx context.Context, id string) (string, error) {
	var createdBy string
	err := r.db.NewSelect().
		Model((*models.Asset)(nil)).
		Column("created_by").
		Where("a.id = ?", id).
		Scan(ctx, &createdBy)
	if err != nil {
		return "", err
	}
	return createdBy, nil
}

func (r *assetRepository) UpdateContent(ctx context.Context, id, title, content string) error {
	_, err := r.db.NewUpdate().
		Model((*models.Asset)(nil)).
		Set("title = ?", title).
		Set("content = ?", content).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

// SetImageResult writes an IMG asset's vision description and (optionally) its
// generated alt text (CON-281). The alt write uses a CASE guard so a user's
// edited alt text (alt_text_edited_by_user = true) is preserved even across a
// re-extraction; content is always overwritten with the latest description.
func (r *assetRepository) SetImageResult(ctx context.Context, id, content, altText string, setAlt bool) error {
	q := r.db.NewUpdate().
		Model((*models.Asset)(nil)).
		Set("content = ?", content).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id)
	if setAlt {
		q = q.Set("alt_text = CASE WHEN alt_text_edited_by_user THEN alt_text ELSE ? END", altText)
	}
	_, err := q.Exec(ctx)
	return err
}

// SetAltText sets an image asset's alt text unconditionally (CON-281).
func (r *assetRepository) SetAltText(ctx context.Context, id, altText string) error {
	_, err := r.db.NewUpdate().
		Model((*models.Asset)(nil)).
		Set("alt_text = ?", altText).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

// ErrUnknownTag is returned by ApplyTags when an `add` id doesn't resolve to a
// tag in the caller's tenant — the request references a tag that doesn't exist
// or belongs to another tenant. Handlers map it to a 400. `remove` ids are not
// validated: dropping an unknown id is a harmless no-op, and rejecting it would
// block cleaning up references to an already-deleted tag.
var ErrUnknownTag = errors.New("unknown tag id")

func (r *assetRepository) ApplyTags(ctx context.Context, assetIDs, add, remove []string) ([]models.Asset, error) {
	if len(assetIDs) == 0 {
		return []models.Asset{}, nil
	}
	// Validate the tags being added before persisting anything: GetByIDs is
	// tenant-scoped by the TenantScoped hooks, so an id that's missing from the
	// result is either nonexistent or another tenant's — reject rather than
	// silently write a dangling id into the asset's tag_ids.
	if len(add) > 0 {
		tags, err := r.tagRepo.GetByIDs(ctx, add)
		if err != nil {
			return nil, err
		}
		for _, id := range add {
			if _, ok := tags[id]; !ok {
				return nil, fmt.Errorf("%w: %s", ErrUnknownTag, id)
			}
		}
	}
	var updated []models.Asset
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// The TenantScoped hooks scope this to the caller's tenant, so a request
		// naming another tenant's asset id simply doesn't load it — and therefore
		// can't be written below. FOR UPDATE locks the loaded rows so concurrent
		// bulk-tag calls on the same assets serialise their read-merge-write and
		// don't clobber each other's tag_ids (lost update).
		var assets []models.Asset
		if err := tx.NewSelect().
			Model(&assets).
			Where("a.id IN (?)", bun.In(assetIDs)).
			For("UPDATE").
			Scan(ctx); err != nil {
			return err
		}
		now := time.Now().UTC()
		for i := range assets {
			assets[i].TagIDs = mergeTagIDs(assets[i].TagIDs, add, remove)
			assets[i].UpdatedAt = now
			// Only the tag set and its timestamp — never title/content, so this
			// can't disturb an in-flight document edit or trigger a re-embed.
			if _, err := tx.NewUpdate().
				Model(&assets[i]).
				Column("tag_ids", "updated_at").
				WherePK().
				Exec(ctx); err != nil {
				return err
			}
		}
		updated = assets
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := r.hydrateTags(ctx, updated); err != nil {
		return nil, err
	}
	if err := r.hydrateFiles(ctx, updated); err != nil {
		return nil, err
	}
	// Never return nil: when assetIDs are all outside the tenant nothing loads,
	// and the handler's c.JSON(updated) must emit [] rather than null.
	if updated == nil {
		updated = []models.Asset{}
	}
	return updated, nil
}

// mergeTagIDs returns existing minus remove, then add appended for any id not
// already present — deduped, preserving existing order with adds last. It never
// returns nil, so the notnull jsonb column always receives an array.
func mergeTagIDs(existing models.StringSlice, add, remove []string) models.StringSlice {
	drop := make(map[string]struct{}, len(remove))
	for _, id := range remove {
		drop[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(existing)+len(add))
	out := make(models.StringSlice, 0, len(existing)+len(add))
	keep := func(id string) {
		if _, dropped := drop[id]; dropped {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, id := range existing {
		keep(id)
	}
	for _, id := range add {
		keep(id)
	}
	return out
}

// Delete removes the asset and, in the same transaction, scrubs its id from
// every campaign.asset_ids and post.used_asset_ids (CON-214). Without the
// scrub a deleted asset lingers as a dangling reference that later hard-fails
// content generation ("asset %q not found").
//
// In a tenant context the TenantScoped hooks scope every statement to the
// caller's tenant. Under a system context those hooks add no predicate, so we
// capture the deleted asset's tenant up front and scope the jsonb-scrub updates
// to it explicitly — a system-context delete can never touch another tenant's
// campaigns or posts.
func (r *assetRepository) Delete(ctx context.Context, id string) (bool, error) {
	var deleted bool
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Read the owning tenant before the delete so the cleanup updates below
		// can be scoped to it even when no tenant is in context (system context).
		var tenantID string
		err := tx.NewSelect().
			Model((*models.Asset)(nil)).
			Column("tenant_id").
			Where("id = ?", id).
			Scan(ctx, &tenantID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // asset not found → nothing to delete or scrub
		}
		if err != nil {
			return err
		}

		res, err := tx.NewDelete().Model((*models.Asset)(nil)).Where("id = ?", id).Exec(ctx)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		deleted = n > 0
		if !deleted {
			return nil // nothing removed → no dangling references to scrub
		}

		// jsonb array element removal: `col - <text>` drops matching elements.
		// The @> guard limits each update to rows that actually reference the id;
		// the needle is the id encoded as a JSON scalar so containment matches an
		// array element (e.g. asset_ids @> '"abc"'). The tenant_id predicate keeps
		// the scrub within the deleted asset's tenant under a system context.
		needle, err := json.Marshal(id)
		if err != nil {
			return err
		}
		if _, err := tx.NewUpdate().
			Model((*models.Campaign)(nil)).
			Set("asset_ids = asset_ids - ?", id).
			Where("tenant_id = ?", tenantID).
			Where("asset_ids @> ?::jsonb", string(needle)).
			Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewUpdate().
			Model((*models.Post)(nil)).
			Set("used_asset_ids = used_asset_ids - ?", id).
			Where("tenant_id = ?", tenantID).
			Where("used_asset_ids @> ?::jsonb", string(needle)).
			Exec(ctx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

func (r *assetRepository) hydrateTags(ctx context.Context, assets []models.Asset) error {
	return HydrateTags(ctx, r.tagRepo, assets,
		func(p models.Asset) []string { return p.TagIDs },
		func(p *models.Asset) *[]models.Tag { return &p.Tags },
	)
}

func (r *assetRepository) hydrateFiles(ctx context.Context, assets []models.Asset) error {
	if r.fileRepo == nil || len(assets) == 0 {
		return nil
	}
	ids := make([]string, 0, len(assets))
	for _, a := range assets {
		ids = append(ids, a.ID)
	}
	byAsset, err := r.fileRepo.ListByAssetIDs(ctx, ids)
	if err != nil {
		return err
	}
	for i := range assets {
		if f, ok := byAsset[assets[i].ID]; ok {
			assets[i].File = f
		}
	}
	return nil
}
