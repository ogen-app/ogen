package queues

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// RefreshZernioAnalyticsQueue is the recurring analytics-refresh queue
// (CON-93 §6 FR3). Each tick batch-pages Zernio's analytics list
// (source=late), matches returned items back to local posts by
// publisher_post_id, and upserts a snapshot per matched post. It mirrors
// ReconcileScheduledPostsQueue: a marker payload, self-rescheduling at a
// fixed cadence, seeded once at boot.
const RefreshZernioAnalyticsQueue = "refresh_zernio_analytics"

// Defaults applied when the corresponding processor tunable is zero.
const (
	defaultAnalyticsWindowDays = 90
	defaultAnalyticsPageLimit  = 100
	// maxAnalyticsPages hard-caps the pages fetched per tick so a large
	// back-catalog can't make one tick unbounded; the uncovered tail is
	// picked up on subsequent ticks (CON-93 §10).
	maxAnalyticsPages = 20

	// Decay-schedule defaults (CON-236). A post is "due" this tick when it has
	// gone longer than its age bucket's interval since the last check.
	defaultDecayFreshWindow = 48 * time.Hour      // age < this → fresh bucket
	defaultDecayWarmWindow  = 14 * 24 * time.Hour // age < this → warm bucket
	defaultDecayFreshEvery  = time.Hour
	defaultDecayWarmEvery   = 24 * time.Hour
	defaultDecayColdEvery   = 7 * 24 * time.Hour
	// decaySlack absorbs periodic-tick jitter so a post whose bucket interval
	// equals the base cadence isn't skipped every other tick by a few seconds.
	decaySlack = time.Minute
)

// AnalyticsDecay is the age-based refresh cadence (CON-236): the older a post,
// the less often its analytics are re-checked. Zero fields fall back to the
// defaults above; a zero *Every disables gating for that bucket (always due).
type AnalyticsDecay struct {
	FreshWindow time.Duration
	WarmWindow  time.Duration
	FreshEvery  time.Duration
	WarmEvery   time.Duration
	ColdEvery   time.Duration
}

// defaultDecay is applied only when the processor's Decay is entirely unset (a
// directly-constructed processor, e.g. in tests). In production envconfig always
// populates the cadence, so an explicit zero for a bucket is honoured (disabled
// gating for that bucket) rather than being overwritten by a default.
var defaultDecay = AnalyticsDecay{
	FreshWindow: defaultDecayFreshWindow,
	WarmWindow:  defaultDecayWarmWindow,
	FreshEvery:  defaultDecayFreshEvery,
	WarmEvery:   defaultDecayWarmEvery,
	ColdEvery:   defaultDecayColdEvery,
}

// RefreshZernioAnalyticsTask is a marker payload — like
// ReconcileScheduledPostsTask, this queue carries no per-tick data.
type RefreshZernioAnalyticsTask struct{}

// Kind implements river.JobArgs.
func (RefreshZernioAnalyticsTask) Kind() string { return RefreshZernioAnalyticsQueue }

// InsertOpts sets per-kind defaults. Cadence is owned by a River PeriodicJob
// (registered when the Zernio integration is configured). Process never
// returns an error — each tick records its own outcome — so a single attempt
// is correct; idempotent upserts keep a re-run safe (§10). UniqueOpts (active
// states only) prevents overlapping ticks from stacking.
func (RefreshZernioAnalyticsTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

// RefreshZernioAnalyticsProcessor wires one refresh tick. Deps supplies
// the post repo (match map), the analytics repo (upsert target), and the
// Zernio client. Settings records the zernio.analytics.* health keys. Hub
// is optional (CON-93 §8, frontend-deferred) — a nil Hub publishes
// nothing.
type RefreshZernioAnalyticsProcessor struct {
	river.WorkerDefaults[RefreshZernioAnalyticsTask]
	Deps     ZernioDeps
	Settings zernio.SettingsStore
	Hub      eventhub.Hub

	WindowDays int            // analytics lookback window (defaults to 90)
	PageLimit  int            // page size, 1–100 (defaults to 100)
	Decay      AnalyticsDecay // CON-236 age-based re-check cadence
}

// Work is the River entrypoint; it delegates to Process.
func (p *RefreshZernioAnalyticsProcessor) Work(ctx context.Context, job *river.Job[RefreshZernioAnalyticsTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	// CON-97: background jobs span tenants (interim until per-tenant, PR4).
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *RefreshZernioAnalyticsProcessor) Timeout(*river.Job[RefreshZernioAnalyticsTask]) time.Duration {
	return 60 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &RefreshZernioAnalyticsProcessor{
			Deps:       d.Zernio,
			Settings:   d.AnalyticsSettings,
			Hub:        d.AnalyticsHub,
			WindowDays: d.AnalyticsWindowDays,
			Decay:      d.AnalyticsDecay,
		})
	})
}

// Process runs one refresh tick across every tenant that owns Zernio-published
// posts. Per-tenant health (zernio.analytics.last_refresh_*) is recorded inside
// refresh under each tenant's context; the aggregate error here only drives the
// tick-level metric and log line.
func (p *RefreshZernioAnalyticsProcessor) Process(ctx context.Context, _ RefreshZernioAnalyticsTask) error {
	tickStart := time.Now()
	upserts, err := p.refresh(ctx, tickStart.UTC())

	if err != nil {
		// Any failure (transient 429/5xx/network, 401, legacy 402, terminal
		// 4xx) is recorded and logged; the tick still reschedules below and
		// returns nil. Transient failures recover on the next cadence tick
		// rather than via a River retry — see InsertOpts for why returning an
		// error here would risk killing or fanning out the recurring chain.
		jobs.ZernioAnalyticsRefreshFailed.Add(1)
		slog.ErrorContext(ctx, "analytics refresh failed", logging.AttrComponent, "jobs.refresh_analytics", "upserts", upserts, "tick_ms", time.Since(tickStart).Milliseconds(), logging.AttrError, err)
	} else {
		jobs.ZernioAnalyticsRefreshSucceeded.Add(1)
		jobs.ZernioAnalyticsPostsUpserted.Add(int64(upserts))
		slog.InfoContext(ctx, "analytics refresh ok", logging.AttrComponent, "jobs.refresh_analytics", "upserts", upserts, "tick_ms", time.Since(tickStart).Milliseconds())
	}
	return nil
}

// refresh sweeps analytics once per tenant that owns Zernio-published posts,
// scoping each fetch to that tenant's Zernio profile (CON-93 follow-up:
// per-profile collection). A single cross-tenant read of the published-post
// projection yields both the set of tenants to sweep and each tenant's match
// map. Per-tenant health is recorded under that tenant's context; the returned
// error is the first tenant-level failure — Process uses it only for the
// tick-level metric, never propagating to River.
func (p *RefreshZernioAnalyticsProcessor) refresh(ctx context.Context, now time.Time) (int, error) {
	if p.Deps.AnalyticsRepo == nil || p.Deps.PostRepo == nil {
		return 0, errors.New("refresh: analytics dependencies not configured")
	}

	// Enumerate under a system context so the scoping hook doesn't restrict the
	// projection to one tenant — this single read spans every tenant.
	posts, err := p.Deps.PostRepo.ListWithPublisherPostID(tenantctx.WithSystem(ctx))
	if err != nil {
		return 0, fmt.Errorf("refresh: list posts: %w", err)
	}
	if len(posts) == 0 {
		return 0, nil
	}
	platformName := p.platformNames(tenantctx.WithSystem(ctx))

	byTenant := make(map[string][]models.Post)
	for _, post := range posts {
		byTenant[post.TenantID] = append(byTenant[post.TenantID], post)
	}

	total := 0
	var firstErr error
	for _, tenantID := range sortedTenantIDs(byTenant) {
		tctx := tenantctx.With(ctx, tenantID)
		n, swept, terr := p.refreshTenant(tctx, byTenant[tenantID], platformName, now)
		total += n
		// Only record health for tenants we actually swept (or that errored);
		// a profile-less tenant is skipped entirely, so writing "ok" would
		// misreport an unconfigured tenant as healthy.
		if swept || terr != nil {
			p.recordStatus(tctx, terr)
		}
		if terr != nil {
			if firstErr == nil {
				firstErr = terr
			}
			slog.ErrorContext(tctx, "analytics refresh: tenant sweep failed", logging.AttrComponent, "jobs.refresh_analytics", "tenant_id", tenantID, logging.AttrError, terr)
		}
	}
	return total, firstErr
}

// refreshTenant fetches and upserts one tenant's analytics. ctx is already
// tenant-scoped, so the Zernio fetch is filtered to this tenant's profile and
// every write/event lands in the right tenant. The returned swept flag is false
// when the tenant has Zernio-published posts but no profile id recorded — it is
// skipped rather than falling back to an unscoped cross-profile fetch, and the
// caller skips its health write so an unconfigured tenant isn't reported "ok".
func (p *RefreshZernioAnalyticsProcessor) refreshTenant(ctx context.Context, posts []models.Post, platformName map[string]string, now time.Time) (upserts int, swept bool, err error) {
	profileID := ""
	if p.Deps.ProfileID != nil {
		id, rerr := p.Deps.ProfileID(ctx)
		if rerr != nil {
			return 0, false, fmt.Errorf("resolve profile id: %w", rerr)
		}
		profileID = id
	}
	if profileID == "" {
		return 0, false, nil
	}

	// publisher_post_id → post_id match map for this tenant, plus the per-post
	// fields denormalised onto the current-state row. Zernio returns analytics
	// for every late post under the profile; we write only the ones Ogen owns.
	byPublisherID := make(map[string]string, len(posts))
	postByID := make(map[string]models.Post, len(posts))
	for _, post := range posts {
		byPublisherID[post.PublisherPostID] = post.ID
		postByID[post.ID] = post
	}

	// Preload the tenant's current-state rows once: the decay gate reads
	// last_checked_at from them and dedup compares metric keys, both without a
	// per-post query (CON-236). A nil map is fine (every post is a first sight).
	current, cerr := p.Deps.AnalyticsRepo.CurrentByPostID(ctx)
	if cerr != nil {
		return upserts, true, fmt.Errorf("load current analytics: %w", cerr)
	}
	if current == nil {
		current = map[string]*models.PostAnalytics{}
	}

	from := now.AddDate(0, 0, -p.windowDays()).Format("2006-01-02")
	limit := p.pageLimit()

	for page := 1; page <= maxAnalyticsPages; page++ {
		apiStart := time.Now()
		items, pagination, lerr := p.Deps.Client.ListAnalytics(ctx, zernio.AnalyticsQuery{
			Source:    zernio.AnalyticsSourceLate,
			ProfileID: profileID,
			FromDate:  from,
			Limit:     limit,
			Page:      page,
		})
		jobs.ObserveZernioCall(time.Since(apiStart))
		if lerr != nil {
			return upserts, true, lerr
		}

		for i := range items {
			postID, publisherPostID := p.matchPostID(byPublisherID, &items[i])
			if postID == "" {
				continue
			}
			post := postByID[postID]
			prev := current[postID]

			// Decay gate: skip posts whose age bucket says they aren't due yet.
			if !p.due(post, prev, now) {
				jobs.ZernioAnalyticsPostsDueSkipped.Add(1)
				continue
			}

			built, berr := buildCurrent(post, publisherPostID, platformName[post.PlatformID], &items[i])
			if berr != nil {
				slog.ErrorContext(ctx, "analytics build failed", logging.AttrComponent, "jobs.refresh_analytics", "post_id", postID, logging.AttrError, berr)
				continue
			}

			// Dedup: a new history point is written only when the metric key
			// moved (or this is the first sighting). Either way we upsert the
			// current row so last_checked_at (freshness) and the latest breakdown
			// stay current.
			changed := prev == nil || prev.MetricsKey() != built.MetricsKey()
			built.LastCheckedAt = now
			if prev == nil {
				built.FirstSeenAt = now
				built.LastChangedAt = now
			} else {
				built.FirstSeenAt = prev.FirstSeenAt
				if changed {
					built.LastChangedAt = now
				} else {
					built.LastChangedAt = prev.LastChangedAt
				}
			}

			if !changed {
				// Unchanged: just bump the current row (last_checked_at + latest
				// breakdown); no new history point.
				if uerr := p.Deps.AnalyticsRepo.Upsert(ctx, built); uerr != nil {
					slog.ErrorContext(ctx, "analytics upsert failed", logging.AttrComponent, "jobs.refresh_analytics", "post_id", postID, logging.AttrError, uerr)
					continue
				}
				current[postID] = built
				jobs.ZernioAnalyticsPostsUnchanged.Add(1)
				continue
			}

			// Changed: write the current row and append the trend point atomically
			// so a snapshot failure can't leave the current row advanced without
			// its history point (dedup would then hide the change forever).
			id, ierr := models.NewID()
			if ierr != nil {
				slog.ErrorContext(ctx, "analytics id gen failed", logging.AttrComponent, "jobs.refresh_analytics", "post_id", postID, logging.AttrError, ierr)
				continue
			}
			if werr := p.Deps.AnalyticsRepo.UpsertWithSnapshot(ctx, built, built.NewSnapshot(id, now)); werr != nil {
				slog.ErrorContext(ctx, "analytics upsert+snapshot failed", logging.AttrComponent, "jobs.refresh_analytics", "post_id", postID, logging.AttrError, werr)
				continue
			}
			// Keep the in-memory map current so a duplicate item later in the
			// same tick dedups against the value we just wrote.
			current[postID] = built
			upserts++
			p.publishUpdated(ctx, built)
		}

		// Stop paging on the last page, robust to whichever pagination shape the
		// endpoint returns: an explicit last-page number, the cursor-style
		// hasMore flag, or — absent both — a short/empty page.
		if len(items) == 0 {
			break
		}
		if last := pagination.LastPage(); last > 0 {
			if page >= last {
				break
			}
			continue
		}
		if !pagination.HasMore && len(items) < limit {
			break
		}
	}
	return upserts, true, nil
}

// due reports whether a post should be re-checked this tick under the decay
// schedule (CON-236). A post never seen before is always due. Otherwise the
// age bucket (from published_at) picks the interval, and the post is due once
// that much time (minus a small slack for tick jitter) has passed since the
// last check. A zero interval for the bucket means gating is explicitly disabled
// (always due) — reachable only via config, since an entirely-unset Decay is
// replaced with the defaults below.
func (p *RefreshZernioAnalyticsProcessor) due(post models.Post, prev *models.PostAnalytics, now time.Time) bool {
	if prev == nil {
		return true
	}
	// Distinguish "unconfigured" (a directly-built processor, e.g. in tests) from
	// an explicit zero: only the fully-zero struct falls back to defaults, so a
	// configured 0 for a bucket is honoured (always due) rather than defaulted.
	d := p.Decay
	if d == (AnalyticsDecay{}) {
		d = defaultDecay
	}
	var age time.Duration
	if post.PublishedAt != nil {
		age = now.Sub(*post.PublishedAt)
	}
	var every time.Duration
	switch {
	case age < d.FreshWindow:
		every = d.FreshEvery
	case age < d.WarmWindow:
		every = d.WarmEvery
	default:
		every = d.ColdEvery
	}
	if every <= 0 {
		return true
	}
	return now.Sub(prev.LastCheckedAt)+decaySlack >= every
}

// platformNames resolves platform_id → display name once per tick (platforms is
// a small seeded reference table) so each snapshot can carry the denormalised
// platform name (CON-125 Track B). Best-effort: a lookup failure leaves names
// empty rather than failing the tick.
func (p *RefreshZernioAnalyticsProcessor) platformNames(ctx context.Context) map[string]string {
	names := map[string]string{}
	if p.Deps.PlatformRepo == nil {
		return names
	}
	plats, err := p.Deps.PlatformRepo.List(ctx)
	if err != nil {
		slog.WarnContext(ctx, "analytics refresh: platform list failed; platform names left empty", logging.AttrComponent, "jobs.refresh_analytics", logging.AttrError, err)
		return names
	}
	for _, pl := range plats {
		names[pl.ID] = pl.Name
	}
	return names
}

// sortedTenantIDs returns the map keys in deterministic order so per-tenant
// sweeps (and their logs/tests) run in a stable sequence.
func sortedTenantIDs(byTenant map[string][]models.Post) []string {
	ids := make([]string, 0, len(byTenant))
	for id := range byTenant {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// matchPostID resolves a returned item to a local post by either of the
// two ids Zernio exposes (CON-93 §6 step 2). It returns the local post
// id plus the publisher_post_id that matched — that matched key (not
// necessarily item.PostID) is what gets denormalised onto the snapshot,
// so it stays consistent with posts.publisher_post_id.
func (p *RefreshZernioAnalyticsProcessor) matchPostID(byPublisherID map[string]string, item *zernio.AnalyticsItem) (postID, publisherPostID string) {
	if item.PostID != "" {
		if id, ok := byPublisherID[item.PostID]; ok {
			return id, item.PostID
		}
	}
	if item.LatePostID != "" {
		if id, ok := byPublisherID[item.LatePostID]; ok {
			return id, item.LatePostID
		}
	}
	return "", ""
}

// buildCurrent maps a Zernio analytics item onto the current-state row for a
// post (CON-236). publisherPostID is the matched key from the local post (kept
// consistent with posts.publisher_post_id); platformName is the resolved display
// name for the post's platform. The post's publisher/title/published_at are
// denormalised so the overview read needs no cross-DB join. The first_seen/
// last_changed/last_checked timestamps are stamped by the caller (they depend on
// the dedup comparison against the previous row).
func buildCurrent(post models.Post, publisherPostID, platformName string, item *zernio.AnalyticsItem) (*models.PostAnalytics, error) {
	return &models.PostAnalytics{
		PostID:             post.ID,
		PublisherPostID:    publisherPostID,
		Publisher:          post.Publisher,
		Platform:           platformName,
		Title:              post.Title,
		PublishedAt:        post.PublishedAt,
		Impressions:        item.Analytics.Impressions,
		Reach:              item.Analytics.Reach,
		Likes:              item.Analytics.Likes,
		Comments:           item.Analytics.Comments,
		Shares:             item.Analytics.Shares,
		Saves:              item.Analytics.Saves,
		Clicks:             item.Analytics.Clicks,
		Views:              item.Analytics.Views,
		EngagementRate:     item.Analytics.EngagementRate,
		PlatformAnalytics:  mapPlatformAnalytics(item.PlatformAnalytics),
		SyncStatus:         item.SyncStatus,
		MetricsLastUpdated: item.Analytics.LastUpdatedTime(),
	}, nil
}

func mapPlatformAnalytics(in []zernio.PlatformAnalytics) models.PlatformAnalyticsList {
	out := make(models.PlatformAnalyticsList, 0, len(in))
	for _, pa := range in {
		out = append(out, models.PostPlatformAnalytics{
			Platform:        pa.Platform,
			Status:          pa.Status,
			PlatformPostID:  pa.PlatformPostID,
			AccountID:       pa.AccountID,
			AccountUsername: pa.AccountUsername,
			PlatformPostURL: pa.PlatformPostURL,
			SyncStatus:      pa.SyncStatus,
			ErrorMessage:    pa.ErrorMessage,
			ReauthorizeURL:  pa.ReauthorizeURL,
			Analytics: models.PostAnalyticsMetrics{
				Impressions:    pa.Analytics.Impressions,
				Reach:          pa.Analytics.Reach,
				Likes:          pa.Analytics.Likes,
				Comments:       pa.Analytics.Comments,
				Shares:         pa.Analytics.Shares,
				Saves:          pa.Analytics.Saves,
				Clicks:         pa.Analytics.Clicks,
				Views:          pa.Analytics.Views,
				EngagementRate: pa.Analytics.EngagementRate,
			},
		})
	}
	return out
}

// publishUpdated emits the optional post.analytics.updated event
// (CON-93 §8). No-op when no Hub is wired.
func (p *RefreshZernioAnalyticsProcessor) publishUpdated(ctx context.Context, a *models.PostAnalytics) {
	if p.Hub == nil {
		return
	}
	if err := p.Hub.Publish(ctx, eventhub.Event{
		Topic:    fmt.Sprintf("entity:post:%s", a.PostID),
		TenantID: a.TenantID,
		Type:     "post.analytics.updated",
		Payload: map[string]any{
			"post_id":     a.PostID,
			"sync_status": a.SyncStatus,
			"analytics":   a.Metrics(),
		},
	}); err != nil {
		slog.ErrorContext(ctx, "analytics publish event failed", logging.AttrComponent, "jobs.refresh_analytics", logging.AttrError, err)
	}
}

// recordStatus writes the documented zernio.analytics.last_refresh_at /
// last_refresh_status settings. Best-effort: write failures are logged,
// not propagated.
func (p *RefreshZernioAnalyticsProcessor) recordStatus(ctx context.Context, refreshErr error) {
	if p.Settings == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := p.Settings.Set(ctx, zernio.SettingAnalyticsLastRefreshAt, now); err != nil {
		slog.ErrorContext(ctx, "analytics settings write failed", logging.AttrComponent, "jobs.refresh_analytics", "setting", zernio.SettingAnalyticsLastRefreshAt, logging.AttrError, err)
	}
	status := zernio.SyncStatusOK
	if refreshErr != nil {
		status = "error: " + truncateAnalyticsErr(refreshErr.Error())
	}
	if err := p.Settings.Set(ctx, zernio.SettingAnalyticsLastRefreshStatus, status); err != nil {
		slog.ErrorContext(ctx, "analytics settings write failed", logging.AttrComponent, "jobs.refresh_analytics", "setting", zernio.SettingAnalyticsLastRefreshStatus, logging.AttrError, err)
	}
}

func (p *RefreshZernioAnalyticsProcessor) windowDays() int {
	if p.WindowDays > 0 {
		return p.WindowDays
	}
	return defaultAnalyticsWindowDays
}

func (p *RefreshZernioAnalyticsProcessor) pageLimit() int {
	if p.PageLimit > 0 && p.PageLimit <= 100 {
		return p.PageLimit
	}
	return defaultAnalyticsPageLimit
}

func truncateAnalyticsErr(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
