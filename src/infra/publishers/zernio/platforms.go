package zernio

import (
	"context"
	"expvar"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// SupportedPlatform describes one publishable platform as the publish/connect
// code sees it. It is projected from a `platforms` row (CON-292): the hardcoded
// Phase-1 allowlist (supportedPlatforms) and Sqid→slug map (sqidToZernioID)
// that used to live here were deleted — the DB row is now the single source of
// truth, loaded into a process-wide snapshot by InitCatalog.
type SupportedPlatform struct {
	ZernioID string // identifier sent to Zernio (POST body, list filter)
	Label    string // human label for picker UI (platform.name)
	OgenID   string // platform.id (Sqid) — join key for PlatformViews / REST

	// SupportedPostTypes is the subset of Ogen post-type slugs (keys of
	// models.Platform.PostTypes) that Zernio can publish to for this platform.
	SupportedPostTypes []string

	// Enabled is the operator soft on/off switch. Availability surfaces
	// (SupportedPlatforms, LookupSupportedPlatform) hide disabled platforms;
	// resolution lookups (by Sqid / Zernio id) still see them so a post already
	// scheduled to a now-disabled platform resolves its slug and publishes.
	Enabled bool
}

// catalog is an immutable snapshot of the platforms table, swapped atomically
// on refresh. Resolution maps (bySqid / byZernio) index every slugged row;
// `enabled` / enabledByZernio index only enabled rows (CON-292 §11).
type catalog struct {
	all             []SupportedPlatform
	enabled         []SupportedPlatform
	bySqid          map[string]*SupportedPlatform
	byZernio        map[string]*SupportedPlatform
	enabledByZernio map[string]*SupportedPlatform
}

// newCatalog projects platform rows into a snapshot. Rows without a zernio_id
// are skipped for resolution (an operator created the catalog entry but has not
// assigned a slug yet, so it can't publish or be connected).
func newCatalog(rows []models.Platform) *catalog {
	c := &catalog{}
	for _, row := range rows {
		if row.ZernioID == "" {
			continue
		}
		c.all = append(c.all, SupportedPlatform{
			ZernioID:           row.ZernioID,
			Label:              row.Name,
			OgenID:             row.ID,
			SupportedPostTypes: append([]string(nil), row.SupportedPostTypes...),
			Enabled:            row.Enabled,
		})
	}
	// Build index maps only after c.all is final so the pointers stay valid
	// (appending could otherwise reallocate the backing array).
	c.bySqid = make(map[string]*SupportedPlatform, len(c.all))
	c.byZernio = make(map[string]*SupportedPlatform, len(c.all))
	for i := range c.all {
		sp := &c.all[i]
		c.bySqid[sp.OgenID] = sp
		c.byZernio[sp.ZernioID] = sp
		if sp.Enabled {
			c.enabled = append(c.enabled, *sp)
		}
	}
	c.enabledByZernio = make(map[string]*SupportedPlatform, len(c.enabled))
	for i := range c.enabled {
		c.enabledByZernio[c.enabled[i].ZernioID] = &c.enabled[i]
	}
	return c
}

// catalogRefreshInterval bounds the staleness window after an operator edit
// when PlatformAdminService's explicit RefreshCatalog is not wired (or misses).
const catalogRefreshInterval = 60 * time.Second

var (
	// activeCatalog holds the current snapshot. Reads are lock-free. Starts
	// empty; InitCatalog replaces it from the DB at boot.
	activeCatalog atomic.Pointer[catalog]

	// catalogSource is the row source, set once by InitCatalog at boot (before
	// the refresh goroutine and any gRPC-driven refresh), so plain access is
	// race-free.
	catalogSource CatalogSource

	catalogRefreshOK   = expvar.NewInt("ogen_zernio_catalog_refresh_ok")
	catalogRefreshFail = expvar.NewInt("ogen_zernio_catalog_refresh_fail")
)

func init() {
	activeCatalog.Store(newCatalog(nil)) // empty until InitCatalog runs
}

// CatalogSource supplies platform rows to the resolver. Satisfied by
// repository.PlatformRepository (List returns enabled + disabled rows).
type CatalogSource interface {
	List(ctx context.Context) ([]models.Platform, error)
}

// InitCatalog loads the platform catalog from src at boot and starts a periodic
// refresh goroutine bound to ctx. Non-fatal: a failed initial load logs and
// leaves the catalog empty (the ticker retries) so the app still starts —
// consistent with the gRPC server's non-fatal posture (CON-292 §10.1). Because
// the row source is mandatory for the whole app, a persistent DB outage is a
// boot failure elsewhere, not here.
func InitCatalog(ctx context.Context, src CatalogSource) {
	if err := LoadCatalog(ctx, src); err != nil {
		slog.ErrorContext(ctx, "initial platform catalog load failed; serving empty catalog until refresh",
			logging.AttrComponent, "zernio.catalog", logging.AttrError, err)
	}
	go func() {
		t := time.NewTicker(catalogRefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := RefreshCatalog(ctx); err != nil {
					slog.WarnContext(ctx, "platform catalog refresh failed; keeping previous snapshot",
						logging.AttrComponent, "zernio.catalog", logging.AttrError, err)
				}
			}
		}
	}()
}

// LoadCatalog sets the row source and loads the snapshot synchronously, without
// starting the background refresh goroutine. InitCatalog builds on it; tests
// (and any caller managing its own refresh) use it to populate the catalog from
// a DB without a lingering goroutine.
func LoadCatalog(ctx context.Context, src CatalogSource) error {
	catalogSource = src
	return RefreshCatalog(ctx)
}

// RefreshCatalog reloads the snapshot now. PlatformAdminService (CON-292 §6)
// calls it after every write so operator edits take effect without waiting for
// the periodic tick. A failed reload keeps the previous snapshot.
func RefreshCatalog(ctx context.Context) error {
	src := catalogSource
	if src == nil {
		return nil
	}
	rows, err := src.List(ctx)
	if err != nil {
		catalogRefreshFail.Add(1)
		return err
	}
	activeCatalog.Store(newCatalog(rows))
	catalogRefreshOK.Add(1)
	return nil
}

// SupportedPlatforms returns a copy of the ENABLED catalog entries. Its callers
// are the connect allowlist listing and the /api/platforms publisher
// enrichment, both of which must hide disabled platforms (CON-292 §11).
func SupportedPlatforms() []SupportedPlatform {
	c := activeCatalog.Load()
	out := make([]SupportedPlatform, len(c.enabled))
	copy(out, c.enabled)
	return out
}

// LookupSupportedPlatform returns the ENABLED catalog entry for a Zernio slug,
// or nil. This is the connect/availability gate: a disabled or unknown platform
// returns nil so new connects (zernio.go) and the auto-publish allowlist guard
// reject it.
func LookupSupportedPlatform(zernioID string) *SupportedPlatform {
	c := activeCatalog.Load()
	if sp, ok := c.enabledByZernio[zernioID]; ok {
		cp := *sp
		return &cp
	}
	return nil
}

// LookupSupportedBySqid resolves a platform by its Sqid (platforms.id) across
// ALL rows — enabled and disabled. The publish path relies on this so a post
// already scheduled to a now-disabled platform still resolves its slug and goes
// out (CON-292 §11). Returns nil for an unknown Sqid or a row with no slug.
func LookupSupportedBySqid(sqid string) *SupportedPlatform {
	c := activeCatalog.Load()
	if sp, ok := c.bySqid[sqid]; ok {
		cp := *sp
		return &cp
	}
	return nil
}

// LookupSqidByZernioID resolves a Zernio slug back to the platform Sqid across
// ALL rows (CON-130 convert-to-manual). Returns "" when the slug is unknown.
func LookupSqidByZernioID(zernioID string) string {
	c := activeCatalog.Load()
	if sp, ok := c.byZernio[zernioID]; ok {
		return sp.OgenID
	}
	return ""
}
