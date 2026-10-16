package queues

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

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

// Kind implements JobArgs.
func (SweepZernioOrphansTask) Kind() string { return SweepZernioOrphansQueue }

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
	Deps   ZernioDeps
	Config OrphanSweepConfig
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Process sweeps every tenant profile in-process.
func (p *SweepZernioOrphansProcessor) Process(ctx context.Context, task SweepZernioOrphansTask) error {
	return runLocal(ctx, p.step, SweepZernioOrphansQueue, task, 0, 0, localActions{
		zernio:   p.Deps.Client,
		children: map[string]stepFunc{OrphanSweepProfileKind: p.profileStep},
	})
}

// OrphanSweepProfileKind sweeps one tenant profile; the sweep fans out into
// one per profile, so a failing profile can't starve the rest.
const OrphanSweepProfileKind = "sweep_zernio_orphans_profile"

type orphanProfileArgs struct {
	TenantID  string `json:"tenant_id"`
	ProfileID string `json:"profile_id"`
}

func (p *SweepZernioOrphansProcessor) step(ctx context.Context, _ StepInput) (StepOutput, error) {
	if p.Deps.SocialAccountRepo == nil {
		return done("skipped_unwired")
	}
	profiles, err := p.Deps.SocialAccountRepo.ListActiveTenantProfiles(tenantctx.WithSystem(ctx))
	if err != nil {
		return retry("orphan sweep: list profiles", err)
	}
	children := make([]ChildJob, 0, len(profiles))
	for _, tp := range profiles {
		raw, err := json.Marshal(orphanProfileArgs{TenantID: tp.TenantID, ProfileID: tp.ProfileID})
		if err != nil {
			return StepOutput{}, err
		}
		children = append(children, ChildJob{Kind: OrphanSweepProfileKind, Args: raw})
	}
	return done("fanned_out", children...)
}

// orphanProfileState pages through the profile's queued posts, then withdraws
// the orphans one by one.
type orphanProfileState struct {
	Pages   []zernio.Result `json:"pages,omitempty"`
	Listed  bool            `json:"listed,omitzero"`
	Orphans []orphanRef     `json:"orphans,omitempty"`
	Next    int             `json:"next,omitzero"`
}

type orphanRef struct {
	PublisherPostID string `json:"publisher_post_id"`
	PostID          string `json:"post_id,omitempty"`
}

func (p *SweepZernioOrphansProcessor) profileStep(ctx context.Context, in StepInput) (StepOutput, error) {
	var args orphanProfileArgs
	if err := json.Unmarshal(in.Args, &args); err != nil || args.ProfileID == "" {
		return done("skipped_bad_args")
	}
	ctx = tenantctx.With(ctx, args.TenantID)
	var st orphanProfileState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	if zernioDisabled(in.Result) {
		return done("skipped_zernio_disabled")
	}
	if !st.Listed {
		if in.Result != nil {
			st.Pages = append(st.Pages, zernioResult(in.Result))
		}
		return p.list(ctx, args, &st)
	}
	p.settleOrphan(ctx, args, st.Orphans[st.Next], in.Result)
	st.Next++
	return p.nextOrphan(ctx, &st)
}

// list pages through the profile's queued posts, then picks the orphans: old
// enough, and held by no scheduled Ogen post.
func (p *SweepZernioOrphansProcessor) list(ctx context.Context, args orphanProfileArgs, st *orphanProfileState) (StepOutput, error) {
	pages := p.Config.MaxPages
	if pages <= 0 {
		pages = 20
	}
	var queued []zernio.Job
	next, err := zernio.Replay(st.Pages, func(c *zernio.Client) error {
		var err error
		queued, err = c.ListScheduled(ctx, args.ProfileID, pages)
		return err
	})
	if next != nil {
		return nextZernio(st, *next)
	}
	if err != nil {
		slog.WarnContext(ctx, "orphan sweep: profile skipped", logging.AttrComponent, "jobs.orphan_sweep",
			"profile_id", args.ProfileID, logging.AttrError, err)
		return done("skipped_list_failed")
	}
	candidates := p.oldEnough(queued)
	if len(candidates) == 0 {
		return done("no_orphans")
	}
	ids := make([]string, len(candidates))
	for i, j := range candidates {
		ids[i] = j.ID
	}
	holders, err := p.Deps.PostRepo.ListByPublisherPostIDs(ctx, ids)
	if err != nil {
		return retry("orphan sweep: load holders", err)
	}
	holder := make(map[string]models.Post, len(holders))
	for _, h := range holders {
		holder[h.PublisherPostID] = h
	}
	*st = orphanProfileState{Listed: true}
	for _, j := range candidates {
		post, held := holder[j.ID]
		if held && post.Status == models.PostStatusScheduled {
			continue
		}
		jobs.ZernioOrphansFound.Add(1)
		attrs := []any{logging.AttrComponent, "jobs.orphan_sweep", "profile_id", args.ProfileID,
			"publisher_post_id", j.ID, "post_id", post.ID, "post_status", post.Status, "live", p.Config.Live}
		if !p.Config.Live {
			slog.InfoContext(ctx, "orphan sweep (dry run): Zernio post queued with no scheduled Ogen post", attrs...)
			continue
		}
		st.Orphans = append(st.Orphans, orphanRef{PublisherPostID: j.ID, PostID: post.ID})
	}
	return p.nextOrphan(ctx, st)
}

// nextOrphan withdraws the next orphan, or ends the sweep of the profile.
func (p *SweepZernioOrphansProcessor) nextOrphan(ctx context.Context, st *orphanProfileState) (StepOutput, error) {
	if st.Next >= len(st.Orphans) {
		return done("swept")
	}
	id := st.Orphans[st.Next].PublisherPostID
	call, err := zernio.Plan(func(c *zernio.Client) error { return c.Cancel(ctx, id) })
	if err != nil {
		return StepOutput{}, err
	}
	return nextZernio(st, call)
}

// settleOrphan records one orphan's withdrawal; a failure is left to the next
// tick.
func (p *SweepZernioOrphansProcessor) settleOrphan(ctx context.Context, args orphanProfileArgs, o orphanRef, res *StepResult) {
	cancelErr := zernio.Decode(zernioResult(res), func(c *zernio.Client) error { return c.Cancel(ctx, o.PublisherPostID) })
	published, err := withdraw(ctx, p.Deps, WithdrawZernioPostTask{
		PublisherPostID: o.PublisherPostID,
		PostID:          o.PostID,
		TenantID:        args.TenantID,
		Reason:          WithdrawReasonOrphan,
		Actor:           models.ActorSystem,
	}, cancelErr)
	attrs := []any{logging.AttrComponent, "jobs.orphan_sweep", "profile_id", args.ProfileID,
		"publisher_post_id", o.PublisherPostID, "post_id", o.PostID, "live", p.Config.Live}
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
