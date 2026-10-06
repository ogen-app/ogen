package queues

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// SweepZernioOrphansQueue is the recurring orphan sweep's queue name.
const SweepZernioOrphansQueue = "sweep_zernio_orphans"

// WithdrawReasonOrphan marks a Zernio post the sweep withdrew.
const WithdrawReasonOrphan = "orphan_sweep"

// SweepZernioOrphansTask is a marker payload; the sweep carries no per-tick
// data.
type SweepZernioOrphansTask struct{}

// Kind implements river.JobArgs.
func (SweepZernioOrphansTask) Kind() string { return SweepZernioOrphansQueue }

// InsertOpts: one attempt (the next tick retries), and the active-state
// uniqueness stops overlapping ticks from stacking.
func (SweepZernioOrphansTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

// OrphanSweepConfig tunes the sweep. Live false is a dry run: orphans are
// counted and logged but left in Zernio.
type OrphanSweepConfig struct {
	Live bool
	// MinAge skips Zernio posts younger than this, so a submit whose Ogen
	// write hasn't landed yet is never mistaken for an orphan. Defaults to 1h.
	MinAge time.Duration
	// MaxPages bounds the listing per profile (100 posts a page). Defaults to 20.
	MaxPages int
}

// SweepZernioOrphansProcessor finds posts still queued in Zernio that no
// scheduled Ogen post holds, and withdraws them. It is the backstop for every
// path that takes a post off scheduled without reaching Zernio, including
// orphans left before the paths were fixed. Only the profiles this
// environment's tenants own are listed, so posts of another environment
// sharing the Zernio account are never seen.
type SweepZernioOrphansProcessor struct {
	river.WorkerDefaults[SweepZernioOrphansTask]
	Deps   ZernioDeps
	Config OrphanSweepConfig
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Work is the River entrypoint; it delegates to Process.
func (p *SweepZernioOrphansProcessor) Work(ctx context.Context, job *river.Job[SweepZernioOrphansTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *SweepZernioOrphansProcessor) Timeout(*river.Job[SweepZernioOrphansTask]) time.Duration {
	return 5 * time.Minute
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &SweepZernioOrphansProcessor{Deps: d.Zernio, Config: d.OrphanSweep})
	})
}

// Process sweeps every tenant profile. A failing profile is logged and
// skipped so one bad profile can't starve the rest.
func (p *SweepZernioOrphansProcessor) Process(ctx context.Context, _ SweepZernioOrphansTask) error {
	if p.Deps.Client == nil || p.Deps.SocialAccountRepo == nil {
		return nil
	}
	profiles, err := p.Deps.SocialAccountRepo.ListActiveTenantProfiles(ctx)
	if err != nil {
		return err
	}
	for _, tp := range profiles {
		tctx := tenantctx.With(ctx, tp.TenantID)
		if err := p.sweepProfile(tctx, tp.TenantID, tp.ProfileID); err != nil {
			slog.WarnContext(tctx, "orphan sweep: profile skipped", logging.AttrComponent, "jobs.orphan_sweep",
				"profile_id", tp.ProfileID, logging.AttrError, err)
		}
	}
	return nil
}

func (p *SweepZernioOrphansProcessor) sweepProfile(ctx context.Context, tenantID, profileID string) error {
	pages := p.Config.MaxPages
	if pages <= 0 {
		pages = 20
	}
	queued, err := p.Deps.Client.ListScheduled(ctx, profileID, pages)
	if err != nil {
		return err
	}
	candidates := p.oldEnough(queued)
	if len(candidates) == 0 {
		return nil
	}
	ids := make([]string, len(candidates))
	for i, j := range candidates {
		ids[i] = j.ID
	}
	holders, err := p.Deps.PostRepo.ListByPublisherPostIDs(ctx, ids)
	if err != nil {
		return err
	}
	holder := make(map[string]models.Post, len(holders))
	for _, h := range holders {
		holder[h.PublisherPostID] = h
	}
	for _, j := range candidates {
		post, held := holder[j.ID]
		if held && post.Status == models.PostStatusScheduled {
			continue
		}
		p.orphan(ctx, tenantID, profileID, j, post)
	}
	return nil
}

// orphan handles one Zernio post no scheduled Ogen post holds. post is the
// zero value when no Ogen post holds the id at all.
func (p *SweepZernioOrphansProcessor) orphan(ctx context.Context, tenantID, profileID string, j zernio.Job, post models.Post) {
	jobs.ZernioOrphansFound.Add(1)
	attrs := []any{logging.AttrComponent, "jobs.orphan_sweep", "profile_id", profileID,
		"publisher_post_id", j.ID, "post_id", post.ID, "post_status", post.Status, "live", p.Config.Live}
	if !p.Config.Live {
		slog.InfoContext(ctx, "orphan sweep (dry run): Zernio post queued with no scheduled Ogen post", attrs...)
		return
	}
	published, err := withdraw(ctx, p.Deps, WithdrawZernioPostTask{
		PublisherPostID: j.ID,
		PostID:          post.ID,
		TenantID:        tenantID,
		Reason:          WithdrawReasonOrphan,
		Actor:           models.ActorSystem,
	})
	switch {
	case err != nil:
		slog.WarnContext(ctx, "orphan sweep: withdraw failed; next tick retries", append(attrs, logging.AttrError, err)...)
	case !published:
		jobs.ZernioOrphansCancelled.Add(1)
		slog.InfoContext(ctx, "orphan sweep: withdrew Zernio post", attrs...)
	}
}

// oldEnough keeps the queued posts created at least MinAge ago. A post without
// a createdAt is skipped: with no age to go on, it might be a submit still in
// flight.
func (p *SweepZernioOrphansProcessor) oldEnough(queued []zernio.Job) []zernio.Job {
	minAge := p.Config.MinAge
	if minAge <= 0 {
		minAge = time.Hour
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	cutoff := now().Add(-minAge)
	var out []zernio.Job
	for _, j := range queued {
		if j.Status != zernio.JobStatusScheduled || j.CreatedAt == nil || j.CreatedAt.After(cutoff) {
			continue
		}
		out = append(out, j)
	}
	return out
}
