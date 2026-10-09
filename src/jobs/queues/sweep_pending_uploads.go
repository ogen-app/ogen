package queues

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// SweepPendingUploadsQueue is the recurring abandoned-upload sweep's queue
// name.
const SweepPendingUploadsQueue = "sweep_pending_uploads"

// SweepPendingUploadsTask is a marker payload; the sweep carries no per-tick
// data.
type SweepPendingUploadsTask struct{}

// Kind implements river.JobArgs.
func (SweepPendingUploadsTask) Kind() string { return SweepPendingUploadsQueue }

// InsertOpts: one attempt (the next tick retries), and the active-state
// uniqueness stops overlapping ticks from stacking.
func (SweepPendingUploadsTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

// PendingUploadSweepConfig tunes the sweep. Live false is a dry run: abandoned
// uploads are counted and logged but left in storage.
type PendingUploadSweepConfig struct {
	Live bool
	// Grace is how long past its URL's expiry an upload waits before it is
	// swept, so a slow upload that started just before expiry can still be
	// finalized. Defaults to 2h.
	Grace time.Duration
	// BatchSize bounds the uploads settled per tick. Defaults to 500.
	BatchSize int
}

// SweepPendingUploadsProcessor deletes the objects of presigned uploads that
// were never finalized: the client closed mid-upload, or finalize failed and
// was not retried. Every tenant is swept, suspended and deleted ones included,
// since this only reclaims storage.
type SweepPendingUploadsProcessor struct {
	river.WorkerDefaults[SweepPendingUploadsTask]
	Repo    repository.PendingUploadRepository
	Storage storage.Storage
	Config  PendingUploadSweepConfig
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Work is the River entrypoint; it delegates to Process.
func (p *SweepPendingUploadsProcessor) Work(ctx context.Context, job *river.Job[SweepPendingUploadsTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *SweepPendingUploadsProcessor) Timeout(*river.Job[SweepPendingUploadsTask]) time.Duration {
	return 5 * time.Minute
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &SweepPendingUploadsProcessor{
			Repo:    d.PendingUploadRepo,
			Storage: d.Zernio.Storage,
			Config:  d.PendingUploadSweep,
		})
	})
}

// Process settles one batch of expired uploads; a backlog drains over the
// following ticks. A failing upload is logged and left for the next tick.
// ctx must be a system context: the listing spans tenants.
func (p *SweepPendingUploadsProcessor) Process(ctx context.Context, _ SweepPendingUploadsTask) error {
	if p.Repo == nil || p.Storage == nil {
		return nil
	}
	cutoff := p.now().Add(-p.grace())
	expired, err := p.Repo.ListExpired(ctx, cutoff, p.Config.BatchSize)
	if err != nil {
		return err
	}
	for _, u := range expired {
		jobs.PendingUploadsFound.Add(1)
		p.settle(ctx, u, cutoff)
	}
	return nil
}

func (p *SweepPendingUploadsProcessor) settle(ctx context.Context, u models.PendingUpload, cutoff time.Time) {
	lctx := tenantctx.With(ctx, u.TenantID)
	attrs := []any{logging.AttrComponent, "jobs.pending_upload_sweep", "pending_upload_id", u.ID,
		"key", u.S3Key, "expires_at", u.ExpiresAt, "live", p.Config.Live}
	if !p.Config.Live {
		slog.InfoContext(lctx, "pending upload sweep (dry run): upload never finalized", attrs...)
		return
	}
	outcome, err := p.Repo.Reap(ctx, u.ID, cutoff, func(ctx context.Context, u models.PendingUpload) error {
		return p.Storage.Delete(ctx, u.S3Key)
	})
	switch {
	case err != nil:
		slog.WarnContext(lctx, "pending upload sweep: delete failed; next tick retries", append(attrs, logging.AttrError, err)...)
	case outcome == repository.ReapRemoved:
		jobs.PendingUploadsSwept.Add(1)
		slog.InfoContext(lctx, "pending upload sweep: deleted abandoned upload", attrs...)
	case outcome == repository.ReapAttached:
		slog.InfoContext(lctx, "pending upload sweep: upload already attached; record dropped", attrs...)
	}
}

func (p *SweepPendingUploadsProcessor) grace() time.Duration {
	if p.Config.Grace <= 0 {
		return 2 * time.Hour
	}
	return p.Config.Grace
}

func (p *SweepPendingUploadsProcessor) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}
