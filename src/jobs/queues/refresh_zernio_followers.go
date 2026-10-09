package queues

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// RefreshZernioFollowersQueue is the recurring follower-stats snapshot queue.
// Each tick sweeps every tenant with active connected accounts,
// fetches follower counts per Zernio profile, and upserts one daily snapshot
// per (account, day). It mirrors RefreshZernioAnalyticsQueue: a marker
// payload, self-rescheduling at a fixed cadence, seeded once at boot.
const RefreshZernioFollowersQueue = "refresh_zernio_followers"

// defaultFollowerBackfillDays is the lookback used on an account's first sweep
// (no snapshots yet). Matches Zernio's own default follower-stats window.
const defaultFollowerBackfillDays = 30

// RefreshZernioFollowersTask is a marker payload — this queue carries no
// per-tick data.
type RefreshZernioFollowersTask struct{}

// Kind implements river.JobArgs.
func (RefreshZernioFollowersTask) Kind() string { return RefreshZernioFollowersQueue }

// InsertOpts mirrors the analytics refresh: one attempt (each tick records its
// own outcome and the upsert is idempotent), active-state uniqueness so
// overlapping ticks don't stack.
func (RefreshZernioFollowersTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

// RefreshZernioFollowersProcessor wires one follower-refresh tick. Deps
// supplies the social-account repo (tenant/profile enumeration), the follower
// repo (upsert target), and the Zernio client. Settings records the
// zernio.followers.* health keys.
type RefreshZernioFollowersProcessor struct {
	river.WorkerDefaults[RefreshZernioFollowersTask]
	Deps     ZernioDeps
	Settings zernio.SettingsStore

	BackfillDays int // first-run lookback (defaults to 30)
}

func (p *RefreshZernioFollowersProcessor) Work(ctx context.Context, job *river.Job[RefreshZernioFollowersTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	// Background jobs span tenants; refreshTenant re-scopes per tenant.
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args)
}

func (p *RefreshZernioFollowersProcessor) Timeout(*river.Job[RefreshZernioFollowersTask]) time.Duration {
	return tenantSweepJobTimeout
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &RefreshZernioFollowersProcessor{
			Deps:     d.Zernio,
			Settings: d.AnalyticsSettings,
		})
	})
}

// Process runs one follower-refresh tick. Like the analytics refresh, per-tenant
// health is recorded inside refresh under each tenant's context; the aggregate
// error here only drives the tick-level metric and log line — it is never
// returned to River (a single attempt records its own outcome and reschedules).
func (p *RefreshZernioFollowersProcessor) Process(ctx context.Context, _ RefreshZernioFollowersTask) error {
	tickStart := time.Now()
	inserted, err := p.refresh(ctx, tickStart.UTC())
	if err != nil {
		jobs.ZernioFollowerRefreshFailed.Add(1)
		slog.ErrorContext(ctx, "follower refresh failed", logging.AttrComponent, "jobs.refresh_followers", "inserted", inserted, "tick_ms", time.Since(tickStart).Milliseconds(), logging.AttrError, err)
	} else {
		jobs.ZernioFollowerRefreshSucceeded.Add(1)
		jobs.ZernioFollowerPointsInserted.Add(int64(inserted))
		slog.InfoContext(ctx, "follower refresh ok", logging.AttrComponent, "jobs.refresh_followers", "inserted", inserted, "tick_ms", time.Since(tickStart).Milliseconds())
	}
	return nil
}

// refresh sweeps follower stats once per tenant that owns active connected
// accounts, scoping each fetch to that tenant's Zernio profile. A single
// cross-tenant read of social_accounts yields the (tenant, profile) pairs to
// sweep. The returned error is the first tenant-level failure — used only for
// the tick metric, never propagated to River.
func (p *RefreshZernioFollowersProcessor) refresh(ctx context.Context, now time.Time) (int, error) {
	if p.Deps.FollowerRepo == nil || p.Deps.SocialAccountRepo == nil || p.Deps.Client == nil {
		return 0, errors.New("refresh followers: dependencies not configured")
	}

	pairs, err := p.Deps.SocialAccountRepo.ListActiveTenantProfiles(tenantctx.WithSystem(ctx))
	if err != nil {
		return 0, fmt.Errorf("refresh followers: list tenant profiles: %w", err)
	}

	var (
		mu       sync.Mutex
		total    int
		firstErr error
	)
	forEachTenant(ctx, pairs, func(ctx context.Context, tp repository.TenantProfile) {
		tctx := tenantctx.With(ctx, tp.TenantID)
		n, terr := p.refreshTenant(tctx, tp.ProfileID, now)
		p.recordStatus(context.WithoutCancel(tctx), terr)
		mu.Lock()
		defer mu.Unlock()
		total += n
		if terr != nil {
			if firstErr == nil {
				firstErr = terr
			}
			slog.ErrorContext(tctx, "follower refresh: tenant sweep failed", logging.AttrComponent, "jobs.refresh_followers", "tenant_id", tp.TenantID, logging.AttrError, terr)
		}
	})
	return total, firstErr
}

// refreshTenant fetches and upserts one tenant's follower stats. ctx is already
// tenant-scoped, so the Zernio fetch is filtered to this tenant's profile and
// every write lands in the right tenant. It requests only points newer than the
// latest snapshot on file (falling back to a bounded backfill on first run).
func (p *RefreshZernioFollowersProcessor) refreshTenant(ctx context.Context, profileID string, now time.Time) (int, error) {
	if profileID == "" {
		return 0, nil
	}

	from := now.AddDate(0, 0, -p.backfillDays()).Format("2006-01-02")
	if latest, err := p.Deps.FollowerRepo.LatestPointDate(ctx); err == nil && !latest.IsZero() {
		from = latest.Format("2006-01-02")
	}

	apiStart := time.Now()
	stats, err := p.Deps.Client.GetFollowerStats(ctx, zernio.FollowerQuery{
		ProfileID:   profileID,
		FromDate:    from,
		Granularity: "daily",
	})
	jobs.ObserveZernioCall(time.Since(apiStart))
	if err != nil {
		return 0, err
	}

	rows := buildFollowerSnapshots(stats, now)
	if len(rows) == 0 {
		return 0, nil
	}
	n, err := p.Deps.FollowerRepo.InsertMany(ctx, rows)
	if err != nil {
		return 0, fmt.Errorf("upsert follower snapshots: %w", err)
	}
	return n, nil
}

// buildFollowerSnapshots flattens a Zernio follower-stats response into daily
// snapshot rows: one per (account, series point). platform/username and the
// window-level growth come from the per-account summary block; the follower
// count is the series datum. Growth is stamped on every row for the account —
// only the latest row is ever read back (Summary picks latest-per-account), so
// older rows' growth values are inert.
func buildFollowerSnapshots(stats zernio.FollowerStats, now time.Time) []models.FollowerSnapshot {
	summaryByID := make(map[string]zernio.FollowerAccount, len(stats.Accounts))
	for _, a := range stats.Accounts {
		summaryByID[a.ID] = a
	}
	var rows []models.FollowerSnapshot
	for accountID, points := range stats.Stats {
		// Skip an account whose series has no matching summary block — without
		// it platform/username and growth are unknown, and stamping the
		// zero-value would store a misleading "no growth" row.
		acc, ok := summaryByID[accountID]
		if !ok {
			continue
		}
		for _, pt := range points {
			pd, perr := time.Parse("2006-01-02", pt.Date)
			if perr != nil {
				continue // skip an unparseable date rather than fail the batch
			}
			id, ierr := models.NewID()
			if ierr != nil {
				continue
			}
			rows = append(rows, models.FollowerSnapshot{
				ID:               id,
				SocialAccountID:  accountID,
				Platform:         acc.Platform,
				Username:         acc.Username,
				Followers:        pt.Followers,
				Growth:           acc.Growth,
				GrowthPercentage: acc.GrowthPercentage,
				PointDate:        pd.UTC(),
				RecordedAt:       now,
			})
		}
	}
	return rows
}

// recordStatus writes the zernio.followers.last_refresh_at / last_refresh_status
// settings, mirroring the analytics refresh. Best-effort.
func (p *RefreshZernioFollowersProcessor) recordStatus(ctx context.Context, refreshErr error) {
	if p.Settings == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := p.Settings.Set(ctx, zernio.SettingFollowersLastRefreshAt, now); err != nil {
		slog.ErrorContext(ctx, "follower settings write failed", logging.AttrComponent, "jobs.refresh_followers", "setting", zernio.SettingFollowersLastRefreshAt, logging.AttrError, err)
	}
	status := zernio.SyncStatusOK
	if refreshErr != nil {
		status = "error: " + truncateAnalyticsErr(refreshErr.Error())
	}
	if err := p.Settings.Set(ctx, zernio.SettingFollowersLastRefreshStatus, status); err != nil {
		slog.ErrorContext(ctx, "follower settings write failed", logging.AttrComponent, "jobs.refresh_followers", "setting", zernio.SettingFollowersLastRefreshStatus, logging.AttrError, err)
	}
}

func (p *RefreshZernioFollowersProcessor) backfillDays() int {
	if p.BackfillDays > 0 {
		return p.BackfillDays
	}
	return defaultFollowerBackfillDays
}
