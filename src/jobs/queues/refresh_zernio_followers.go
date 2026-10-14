package queues

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
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
	return p.Process(ctx, job.Args)
}

func (p *RefreshZernioFollowersProcessor) Timeout(*river.Job[RefreshZernioFollowersTask]) time.Duration {
	return tenantSweepJobTimeout
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, newRefreshZernioFollowers(d))
	})
}

func newRefreshZernioFollowers(d Deps) *RefreshZernioFollowersProcessor {
	return &RefreshZernioFollowersProcessor{Deps: d.Zernio, Settings: d.AnalyticsSettings}
}

// FollowersTenantKind refreshes one tenant's follower stats; the tick fans
// out into one per tenant with active connected accounts.
const FollowersTenantKind = "refresh_zernio_followers_tenant"

type followersTenantArgs struct {
	TenantID  string `json:"tenant_id"`
	ProfileID string `json:"profile_id"`
}

// Process runs one follower-refresh tick in-process, tenant by tenant.
func (p *RefreshZernioFollowersProcessor) Process(ctx context.Context, task RefreshZernioFollowersTask) error {
	return runLocal(ctx, p.step, RefreshZernioFollowersQueue, task, 0, 0, localActions{
		zernio:   p.Deps.Client,
		children: map[string]stepFunc{FollowersTenantKind: p.tenantStep},
	})
}

// step fans the tick out across the (tenant, profile) pairs with active
// connected accounts. Each tenant's job records its own health.
func (p *RefreshZernioFollowersProcessor) step(ctx context.Context, _ StepInput) (StepOutput, error) {
	if p.Deps.FollowerRepo == nil || p.Deps.SocialAccountRepo == nil {
		return done("skipped_unwired")
	}
	pairs, err := p.Deps.SocialAccountRepo.ListActiveTenantProfiles(tenantctx.WithSystem(ctx))
	if err != nil {
		jobs.ZernioFollowerRefreshFailed.Add(1)
		slog.ErrorContext(ctx, "follower refresh failed", logging.AttrComponent, "jobs.refresh_followers",
			logging.AttrError, fmt.Errorf("refresh followers: list tenant profiles: %w", err))
		return done("failed_list")
	}
	children := make([]ChildJob, 0, len(pairs))
	for _, tp := range pairs {
		raw, err := json.Marshal(followersTenantArgs{TenantID: tp.TenantID, ProfileID: tp.ProfileID})
		if err != nil {
			return StepOutput{}, err
		}
		children = append(children, ChildJob{Kind: FollowersTenantKind, Args: raw})
	}
	return done("fanned_out", children...)
}

// tenantStep fetches and upserts one tenant's follower stats in one call,
// scoped to its Zernio profile. It requests only points newer than the latest
// snapshot on file (falling back to a bounded backfill on first run).
func (p *RefreshZernioFollowersProcessor) tenantStep(ctx context.Context, in StepInput) (StepOutput, error) {
	var args followersTenantArgs
	if err := json.Unmarshal(in.Args, &args); err != nil || args.TenantID == "" {
		return done("skipped_bad_args")
	}
	if p.Deps.FollowerRepo == nil {
		return done("skipped_unwired")
	}
	ctx = tenantctx.With(ctx, args.TenantID)
	if zernioDisabled(in.Result) {
		return done("skipped_zernio_disabled")
	}
	now := time.Now().UTC()
	if args.ProfileID == "" {
		return p.tenantDone(ctx, 0, nil)
	}
	from := now.AddDate(0, 0, -p.backfillDays()).Format("2006-01-02")
	if latest, err := p.Deps.FollowerRepo.LatestPointDate(ctx); err == nil && !latest.IsZero() {
		from = latest.Format("2006-01-02")
	}
	q := zernio.FollowerQuery{ProfileID: args.ProfileID, FromDate: from, Granularity: "daily"}
	var stats zernio.FollowerStats
	fetch := func(c *zernio.Client) error {
		var err error
		stats, err = c.GetFollowerStats(ctx, q)
		return err
	}
	if in.Result == nil {
		call, err := zernio.Plan(fetch)
		if err != nil {
			return StepOutput{}, err
		}
		return nextZernio(nil, call)
	}
	if err := zernio.Decode(zernioResult(in.Result), fetch); err != nil {
		return p.tenantDone(ctx, 0, err)
	}
	rows := buildFollowerSnapshots(stats, now)
	if len(rows) == 0 {
		return p.tenantDone(ctx, 0, nil)
	}
	n, err := p.Deps.FollowerRepo.InsertMany(ctx, rows)
	if err != nil {
		return p.tenantDone(ctx, 0, fmt.Errorf("upsert follower snapshots: %w", err))
	}
	return p.tenantDone(ctx, n, nil)
}

// tenantDone records the tenant's health and ends its job; a failure
// recovers on the next tick.
func (p *RefreshZernioFollowersProcessor) tenantDone(ctx context.Context, inserted int, err error) (StepOutput, error) {
	sctx, cancel := statusWriteContext(ctx)
	p.recordStatus(sctx, err)
	cancel()
	if err != nil {
		jobs.ZernioFollowerRefreshFailed.Add(1)
		slog.ErrorContext(ctx, "follower refresh: tenant sweep failed", logging.AttrComponent, "jobs.refresh_followers", logging.AttrError, err)
		return done("failed")
	}
	jobs.ZernioFollowerRefreshSucceeded.Add(1)
	jobs.ZernioFollowerPointsInserted.Add(int64(inserted))
	slog.InfoContext(ctx, "follower refresh ok", logging.AttrComponent, "jobs.refresh_followers", "inserted", inserted)
	return done("refreshed")
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
