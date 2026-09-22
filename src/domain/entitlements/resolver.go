package entitlements

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// VersionStore is the read surface over tier versions the resolver needs.
// Satisfied structurally by repository.TenantTierVersionRepository, so this
// domain package never imports infra/repository.
type VersionStore interface {
	GetByID(ctx context.Context, id string) (*models.TenantTierVersion, error)
	LatestActiveByTier(ctx context.Context, tierID string) (*models.TenantTierVersion, error)
	PricesByVersion(ctx context.Context, versionID string) ([]models.TenantTierVersionPrice, error)
}

// AssignmentStore reads a tenant's tier-version assignment history.
type AssignmentStore interface {
	CoveringAt(ctx context.Context, tenantID string, at time.Time) (*models.TenantTierAssignment, error)
}

// TenantStore reads the tenant's current tier — the resolver's fallback when a
// tenant has no explicit assignment yet.
type TenantStore interface {
	GetByID(ctx context.Context, id string) (*models.Tenant, error)
}

// EntitlementValue is one catalog feature enriched with the value a version
// grants for it: a number, a bool, or nil = unlimited for a numeric feature.
type EntitlementValue struct {
	Feature
	Value any `json:"value"`
	// Current is the tenant's live usage for a numeric feature that has a
	// registered counter (CON-295), attached by the authenticated /me/entitlements
	// read so the client can render "N of M". Omitted (nil) for an uncounted or
	// boolean feature and for the public pricing catalog (no tenant) — the client
	// treats absent as "unknown", distinct from a real 0.
	Current *int64 `json:"current,omitempty"`
}

// Resolution is the fully-resolved entitlement picture for a tenant (or a single
// tier version): the version in force plus its prices and enriched entitlements.
type Resolution struct {
	TierID       string                          `json:"tier_id"`
	VersionID    string                          `json:"version_id"`
	Version      int                             `json:"version"`
	Status       string                          `json:"status"`
	Purchasable  bool                            `json:"purchasable"`
	ChangeReason string                          `json:"change_reason"`
	Prices       []models.TenantTierVersionPrice `json:"prices"`
	Entitlements []EntitlementValue              `json:"entitlements"`
}

// Enrich joins a tier version's stored entitlement values with catalog metadata,
// producing a stable, catalog-ordered view. Version keys absent from the catalog
// are skipped (the validator rejects those at write time); catalog features
// absent from the version are omitted.
func Enrich(cat *Catalog, v *models.TenantTierVersion, prices []models.TenantTierVersionPrice) Resolution {
	ents := make([]EntitlementValue, 0, len(v.Entitlements))
	for _, f := range cat.Features() {
		val, ok := v.Entitlements[f.Key]
		if !ok {
			continue
		}
		ents = append(ents, EntitlementValue{Feature: f, Value: val})
	}
	return Resolution{
		TierID:       v.TierID,
		VersionID:    v.ID,
		Version:      v.Version,
		Status:       v.Status,
		Purchasable:  v.Purchasable,
		ChangeReason: v.ChangeReason,
		Prices:       prices,
		Entitlements: ents,
	}
}

type versionBundle struct {
	version *models.TenantTierVersion
	prices  []models.TenantTierVersionPrice
}

// Resolver answers "which tier version, and what entitlements, is a tenant on at
// time T" (CON-243 §8). Version content is immutable, so a version bundle is
// cached forever once loaded; the per-tenant assignment is read fresh each call,
// so a reassignment is picked up with no cache invalidation.
type Resolver struct {
	versions    VersionStore
	assignments AssignmentStore
	tenants     TenantStore
	catalog     *Catalog
	cache       sync.Map // version id -> *versionBundle
}

// NewResolver builds a Resolver over the given stores and catalog.
func NewResolver(versions VersionStore, assignments AssignmentStore, tenants TenantStore, catalog *Catalog) *Resolver {
	return &Resolver{versions: versions, assignments: assignments, tenants: tenants, catalog: catalog}
}

// Resolve returns the entitlement picture in force for tenantID at the instant
// at, from the assignment covering that instant. It does NOT fall back to the
// tier's current version: for a historical at with no recorded assignment there
// is genuinely no version in force then, so sql.ErrNoRows is returned rather than
// fabricating the present. For "what is this tenant on right now", use
// ResolveCurrent.
func (r *Resolver) Resolve(ctx context.Context, tenantID string, at time.Time) (*Resolution, error) {
	asg, err := r.assignments.CoveringAt(ctx, tenantID, at)
	if err != nil {
		return nil, err
	}
	return r.resolveVersion(ctx, asg.TierVersionID)
}

// ResolveCurrent returns the entitlement picture in force right now. When the
// tenant has no explicit assignment yet — not backfilled, or a brand-new signup
// — it falls back to the tenant's current tier's latest active version. That
// fallback is sound only for the present (it is the tier's *current* version),
// which is why Resolve does not apply it to historical queries.
func (r *Resolver) ResolveCurrent(ctx context.Context, tenantID string) (*Resolution, error) {
	asg, err := r.assignments.CoveringAt(ctx, tenantID, time.Now().UTC())
	switch {
	case err == nil:
		return r.resolveVersion(ctx, asg.TierVersionID)
	case errors.Is(err, sql.ErrNoRows):
		t, terr := r.tenants.GetByID(ctx, tenantID)
		if terr != nil {
			return nil, terr
		}
		v, verr := r.versions.LatestActiveByTier(ctx, t.TierID)
		if verr != nil {
			return nil, verr
		}
		return r.resolveVersion(ctx, v.ID)
	default:
		return nil, err
	}
}

func (r *Resolver) resolveVersion(ctx context.Context, versionID string) (*Resolution, error) {
	b, err := r.loadVersion(ctx, versionID)
	if err != nil {
		return nil, err
	}
	res := Enrich(r.catalog, b.version, b.prices)
	return &res, nil
}

// loadVersion returns a version + its prices, caching the immutable bundle by
// version id so the cache-hit path never touches the entitlement tables.
func (r *Resolver) loadVersion(ctx context.Context, id string) (*versionBundle, error) {
	if cached, ok := r.cache.Load(id); ok {
		return cached.(*versionBundle), nil
	}
	version, err := r.versions.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	prices, err := r.versions.PricesByVersion(ctx, id)
	if err != nil {
		return nil, err
	}
	actual, _ := r.cache.LoadOrStore(id, &versionBundle{version: version, prices: prices})
	return actual.(*versionBundle), nil
}
