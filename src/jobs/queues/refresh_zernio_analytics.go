package queues

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/ogen-app/ogen/src/analytics/tracking"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// RefreshZernioAnalyticsQueue is the recurring analytics-refresh queue.
// Each tick batch-pages Zernio's analytics list
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
	// picked up on subsequent ticks.
	maxAnalyticsPages = 20

	// Decay-schedule defaults. A post is "due" this tick when it has
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

// AnalyticsDecay is the age-based refresh cadence: the older a post,
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

// Kind implements JobArgs.
func (RefreshZernioAnalyticsTask) Kind() string { return RefreshZernioAnalyticsQueue }

// RefreshZernioAnalyticsProcessor wires one refresh tick. Deps supplies
// the post repo (match map), the analytics repo (upsert target), and the
// Zernio client. Settings records the zernio.analytics.* health keys. Hub
// is optional (CON-93 §8, frontend-deferred) — a nil Hub publishes
// nothing.
type RefreshZernioAnalyticsProcessor struct {
	Deps     ZernioDeps
	Settings zernio.SettingsStore
	Hub      eventhub.Hub

	WindowDays int            // analytics lookback window (defaults to 90)
	PageLimit  int            // page size, 1–100 (defaults to 100)
	Decay      AnalyticsDecay // CON-236 age-based re-check cadence
}

func newRefreshZernioAnalytics(d Deps) *RefreshZernioAnalyticsProcessor {
	return &RefreshZernioAnalyticsProcessor{
		Deps:       d.Zernio,
		Settings:   d.AnalyticsSettings,
		Hub:        d.AnalyticsHub,
		WindowDays: d.AnalyticsWindowDays,
		Decay:      d.AnalyticsDecay,
	}
}

// AnalyticsTenantKind refreshes one tenant's analytics; the tick fans out
// into one per tenant that owns Zernio-published posts.
const AnalyticsTenantKind = "refresh_zernio_analytics_tenant"

type analyticsTenantArgs struct {
	TenantID string `json:"tenant_id"`
}

// Process runs one refresh tick in-process, tenant by tenant.
func (p *RefreshZernioAnalyticsProcessor) Process(ctx context.Context, task RefreshZernioAnalyticsTask) error {
	return runLocal(ctx, p.step, RefreshZernioAnalyticsQueue, task, 0, 0, localActions{
		zernio:   p.Deps.Client,
		children: map[string]stepFunc{AnalyticsTenantKind: p.tenantStep},
	})
}

// step fans the tick out across every tenant that owns Zernio-published
// posts. Per-tenant health (zernio.analytics.last_refresh_*) is recorded by
// each tenant's job.
func (p *RefreshZernioAnalyticsProcessor) step(ctx context.Context, _ StepInput) (StepOutput, error) {
	if p.Deps.AnalyticsRepo == nil || p.Deps.PostRepo == nil {
		return done("skipped_unwired")
	}
	posts, err := p.Deps.PostRepo.ListWithPublisherPostID(tenantctx.WithSystem(ctx))
	if err != nil {
		jobs.ZernioAnalyticsRefreshFailed.Add(1)
		slog.ErrorContext(ctx, "analytics refresh failed", logging.AttrComponent, "jobs.refresh_analytics", logging.AttrError, err)
		return done("failed_list")
	}
	byTenant := make(map[string][]models.Post)
	for _, post := range posts {
		byTenant[post.TenantID] = append(byTenant[post.TenantID], post)
	}
	tenants := sortedTenantIDs(byTenant)
	children := make([]ChildJob, 0, len(tenants))
	for _, id := range tenants {
		raw, err := json.Marshal(analyticsTenantArgs{TenantID: id})
		if err != nil {
			return StepOutput{}, err
		}
		children = append(children, ChildJob{Kind: AnalyticsTenantKind, Args: raw})
	}
	return done("fanned_out", children...)
}

// analyticsTenantState pages through one tenant's analytics.
type analyticsTenantState struct {
	ProfileID string    `json:"profile_id"`
	From      string    `json:"from"`
	Page      int       `json:"page"`
	Now       time.Time `json:"now"`
	Upserts   int       `json:"upserts,omitzero"`
}

// tenantStep fetches and upserts one tenant's analytics, a page per step,
// scoped to the tenant's Zernio profile. A tenant with Zernio-published posts
// but no profile id recorded is skipped rather than falling back to an
// unscoped cross-profile fetch, and gets no health write, so an unconfigured
// tenant isn't reported "ok".
func (p *RefreshZernioAnalyticsProcessor) tenantStep(ctx context.Context, in StepInput) (StepOutput, error) {
	var args analyticsTenantArgs
	if err := json.Unmarshal(in.Args, &args); err != nil || args.TenantID == "" {
		return done("skipped_bad_args")
	}
	if p.Deps.AnalyticsRepo == nil || p.Deps.PostRepo == nil {
		return done("skipped_unwired")
	}
	ctx = tenantctx.With(ctx, args.TenantID)
	var st analyticsTenantState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	if zernioDisabled(in.Result) {
		return done("skipped_zernio_disabled")
	}
	posts, err := p.tenantPosts(ctx, args.TenantID)
	if err != nil {
		return p.tenantDone(ctx, &st, err)
	}
	// The tenant's current-state rows: the decay gate reads last_checked_at
	// from them and dedup compares metric keys, both without a per-post
	// query. A nil map is fine (every post is a first sight).
	current, err := p.Deps.AnalyticsRepo.CurrentByPostID(ctx)
	if err != nil {
		return p.tenantDone(ctx, &st, fmt.Errorf("load current analytics: %w", err))
	}
	if in.Result == nil {
		st.Now = time.Now().UTC()
		if p.Deps.ProfileID != nil {
			if st.ProfileID, err = p.Deps.ProfileID(ctx); err != nil {
				return p.tenantDone(ctx, &st, fmt.Errorf("resolve profile id: %w", err))
			}
		}
		if st.ProfileID == "" {
			return done("skipped_no_profile")
		}
		var anyDue bool
		if st.From, anyDue = p.fetchFrom(posts, current, st.Now); !anyDue {
			return p.tenantDone(ctx, &st, nil) // every post was checked recently enough
		}
		st.Page = 1
		return p.nextAnalyticsPage(ctx, &st)
	}

	var items []zernio.AnalyticsItem
	var pagination zernio.AnalyticsPagination
	err = zernio.Decode(zernioResult(in.Result), func(c *zernio.Client) error {
		var err error
		items, pagination, err = c.ListAnalytics(ctx, p.analyticsQuery(&st))
		return err
	})
	if err != nil {
		return p.tenantDone(ctx, &st, err)
	}
	sweep := newAnalyticsSweep(posts, current, p.platformNames(tenantctx.WithSystem(ctx)), st.Now)
	for i := range items {
		if p.applyItem(ctx, sweep, &items[i]) {
			st.Upserts++
		}
	}
	if st.Page >= maxAnalyticsPages || shouldStopPaging(st.Page, len(items), p.pageLimit(), pagination) {
		return p.tenantDone(ctx, &st, nil)
	}
	st.Page++
	return p.nextAnalyticsPage(ctx, &st)
}

// tenantPosts is the tenant's Zernio-published posts. The query runs under
// the tenant's context, so the scoping hook keeps it to that tenant's rows.
// Steps hold no state between calls, so each result page reads it again.
func (p *RefreshZernioAnalyticsProcessor) tenantPosts(ctx context.Context, tenantID string) ([]models.Post, error) {
	posts, err := p.Deps.PostRepo.ListWithPublisherPostID(tenantctx.With(ctx, tenantID))
	if err != nil {
		return nil, fmt.Errorf("refresh: list posts: %w", err)
	}
	return posts, nil
}

func (p *RefreshZernioAnalyticsProcessor) analyticsQuery(st *analyticsTenantState) zernio.AnalyticsQuery {
	return zernio.AnalyticsQuery{
		Source:    zernio.AnalyticsSourceLate,
		ProfileID: st.ProfileID,
		FromDate:  st.From,
		Limit:     p.pageLimit(),
		Page:      st.Page,
	}
}

func (p *RefreshZernioAnalyticsProcessor) nextAnalyticsPage(ctx context.Context, st *analyticsTenantState) (StepOutput, error) {
	q := p.analyticsQuery(st)
	call, err := zernio.Plan(func(c *zernio.Client) error { _, _, err := c.ListAnalytics(ctx, q); return err })
	if err != nil {
		return StepOutput{}, err
	}
	return nextZernio(st, call)
}

// tenantDone records the tenant's health and ends its job. Any failure
// (transient 429/5xx/network, 401, legacy 402, terminal 4xx) is recorded and
// logged; it recovers on the next tick rather than via a retry.
func (p *RefreshZernioAnalyticsProcessor) tenantDone(ctx context.Context, st *analyticsTenantState, err error) (StepOutput, error) {
	sctx, cancel := statusWriteContext(ctx)
	p.recordStatus(sctx, err)
	cancel()
	if err != nil {
		jobs.ZernioAnalyticsRefreshFailed.Add(1)
		slog.ErrorContext(ctx, "analytics refresh: tenant sweep failed", logging.AttrComponent, "jobs.refresh_analytics",
			"upserts", st.Upserts, logging.AttrError, err)
		return done("failed")
	}
	jobs.ZernioAnalyticsRefreshSucceeded.Add(1)
	jobs.ZernioAnalyticsPostsUpserted.Add(int64(st.Upserts))
	slog.InfoContext(ctx, "analytics refresh ok", logging.AttrComponent, "jobs.refresh_analytics", "upserts", st.Upserts)
	return done("refreshed")
}

// fetchFrom returns the FromDate a tenant's fetch needs and whether any of its
// posts is due under the decay schedule. Settled posts are re-checked rarely,
// so the fetch starts at the oldest due post's publish day (a day early, for
// time zones) rather than at the start of the window; without that, every
// page of the window was fetched and the decay only skipped the writes.
func (p *RefreshZernioAnalyticsProcessor) fetchFrom(posts []models.Post, current map[string]*models.PostAnalytics, now time.Time) (string, bool) {
	windowStart := now.AddDate(0, 0, -p.windowDays())
	var oldest *time.Time
	anyDue, unknownAge := false, false
	for i := range posts {
		if !p.due(posts[i], current[posts[i].ID], now) {
			continue
		}
		anyDue = true
		switch at := posts[i].PublishedAt; {
		case at == nil:
			unknownAge = true
		case oldest == nil || at.Before(*oldest):
			oldest = at
		}
	}
	from := windowStart
	if !unknownAge && oldest != nil && oldest.AddDate(0, 0, -1).After(windowStart) {
		from = oldest.AddDate(0, 0, -1)
	}
	return from.Format("2006-01-02"), anyDue
}

// analyticsSweep is one tenant's refresh state. Zernio returns analytics for
// every late post under the profile; only posts Ogen owns are written.
type analyticsSweep struct {
	byPublisherID map[string]string // publisher_post_id → post_id
	postByID      map[string]models.Post
	// current is kept up to date as rows are written, so a duplicate item
	// later in the same tick dedups against the value just written.
	current      map[string]*models.PostAnalytics
	platformName map[string]string
	now          time.Time
}

func newAnalyticsSweep(posts []models.Post, current map[string]*models.PostAnalytics, platformName map[string]string, now time.Time) *analyticsSweep {
	s := &analyticsSweep{
		byPublisherID: make(map[string]string, len(posts)),
		postByID:      make(map[string]models.Post, len(posts)),
		current:       current,
		platformName:  platformName,
		now:           now,
	}
	if s.current == nil {
		s.current = map[string]*models.PostAnalytics{}
	}
	for _, post := range posts {
		s.byPublisherID[post.PublisherPostID] = post.ID
		s.postByID[post.ID] = post
	}
	return s
}

// applyItem writes one analytics item for a post Ogen owns, gated by the decay
// schedule. It reports whether a new history point was written; per-post
// failures are logged and skipped.
func (p *RefreshZernioAnalyticsProcessor) applyItem(ctx context.Context, s *analyticsSweep, item *zernio.AnalyticsItem) bool {
	postID, publisherPostID := p.matchPostID(s.byPublisherID, item)
	if postID == "" {
		return false
	}
	post := s.postByID[postID]
	prev := s.current[postID]
	if !p.due(post, prev, s.now) {
		jobs.ZernioAnalyticsPostsDueSkipped.Add(1)
		return false
	}
	built, err := buildCurrent(post, publisherPostID, s.platformName[post.PlatformID], item)
	if err != nil {
		slog.ErrorContext(ctx, "analytics build failed", logging.AttrComponent, "jobs.refresh_analytics", "post_id", postID, logging.AttrError, err)
		return false
	}

	changed, err := tracking.RecordCurrent(ctx, p.Deps.AnalyticsRepo, prev, built, s.now)
	if err != nil {
		slog.ErrorContext(ctx, "analytics record failed", logging.AttrComponent, "jobs.refresh_analytics",
			"post_id", postID, "changed", changed, logging.AttrError, err)
		return false
	}
	s.current[postID] = built
	if !changed {
		jobs.ZernioAnalyticsPostsUnchanged.Add(1)
		return false
	}
	p.publishUpdated(ctx, built)
	return true
}

// shouldStopPaging reports whether page was the last one, robust to whichever
// pagination shape the endpoint returns: an explicit last-page number, the
// cursor-style hasMore flag, or — absent both — a short/empty page.
func shouldStopPaging(page, itemCount, limit int, pagination zernio.AnalyticsPagination) bool {
	if itemCount == 0 {
		return true
	}
	if last := pagination.LastPage(); last > 0 {
		return page >= last
	}
	return !pagination.HasMore && itemCount < limit
}

// due reports whether a post should be re-checked this tick under the decay
// schedule. A post never seen before is always due. Otherwise the
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
// post. publisherPostID is the matched key from the local post (kept
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

// publishUpdated emits the optional post.analytics.updated event.
// No-op when no Hub is wired.
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
