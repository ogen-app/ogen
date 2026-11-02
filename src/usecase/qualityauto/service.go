// Package qualityauto assesses post quality automatically at points in a
// post's lifecycle: when it is marked ready for publish, when a version is
// saved, and when it is scheduled or published. Checkpoints queue the post;
// a periodic sweep runs the assessments that are due.
package qualityauto

import (
	"context"
	"database/sql"
	"errors"
	"expvar"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

const (
	// sweepBatch is how many due posts one sweep claims.
	sweepBatch = 6
	// sweepConcurrency is how many assessments a sweep runs at once; each is
	// four parallel model calls.
	sweepConcurrency = 3
	// claimLease outlasts a sweep, so a live claim is never taken twice; a
	// sweep that dies frees its rows when the lease lapses.
	claimLease = 10 * time.Minute
	// maxAttempts bounds the runs of one checkpoint that failed with a
	// retryable error.
	maxAttempts = 2
	// retryAfter spaces a retry from the failed attempt.
	retryAfter = 2 * time.Minute
)

var (
	metricQueued  = expvar.NewInt("ogen_quality_auto_queued")
	metricRun     = expvar.NewInt("ogen_quality_auto_run")
	metricSkipped = expvar.NewMap("ogen_quality_auto_skipped")
	metricFailed  = expvar.NewInt("ogen_quality_auto_failed")
)

// Outcome is how one automatic assessment ended, as the runner classifies it.
type Outcome int

const (
	// Done: the post was assessed, or the stored assessment still holds.
	Done Outcome = iota
	// Skipped: a cap or a precondition stopped the run; not retried.
	Skipped
	// Failed: a retryable failure (model error, flows unavailable).
	Failed
)

// Runner assesses one post for trigger and classifies the result. The
// sweep's context carries the post's tenant.
type Runner func(ctx context.Context, postID string, trigger models.QualityTrigger) (Outcome, error)

// Config holds the checkpoint delays. A zero Enabled queues nothing.
type Config struct {
	Enabled      bool
	ReadyDelay   time.Duration
	VersionDelay time.Duration
}

// TenantStatusReader reads a tenant's lifecycle status; assessments of a
// suspended or deleted tenant's posts are dropped.
type TenantStatusReader interface {
	GetStatus(ctx context.Context, tenantID string) (string, error)
}

// Service queues automatic assessments and sweeps the due ones. A nil
// *Service is a no-op, so callers wire it optionally.
type Service struct {
	queue   repository.PostQualityQueueRepository
	tenants TenantStatusReader
	cfg     Config
	runner  atomic.Pointer[Runner]
	now     func() time.Time
}

// New builds the service. tenants may be nil (every tenant counts as active).
func New(queue repository.PostQualityQueueRepository, tenants TenantStatusReader, cfg Config) *Service {
	return &Service{queue: queue, tenants: tenants, cfg: cfg, now: time.Now}
}

// SetRunner sets the assessment entry point. The sweep claims nothing until
// it is set.
func (s *Service) SetRunner(r Runner) {
	if s != nil {
		s.runner.Store(&r)
	}
}

// Schedule queues post for an automatic assessment at the trigger's delay.
// A version saved on a post that has left editing (scheduled, published) is
// ignored: its content is frozen and was assessed at the schedule checkpoint.
// Best-effort: a queue failure is logged, never returned, so a checkpoint
// never fails the action that reached it.
func (s *Service) Schedule(ctx context.Context, post *models.Post, trigger models.QualityTrigger) {
	if s == nil || !s.cfg.Enabled || post == nil {
		return
	}
	if trigger == models.QualityTriggerVersion && !editable(post.Status) {
		return
	}
	due := s.now().Add(s.delay(trigger))
	if err := s.queue.Schedule(ctx, post.TenantID, post.ID, trigger, due); err != nil {
		slog.WarnContext(ctx, "queue automatic assessment failed", logging.AttrComponent, "qualityauto", "post_id", post.ID, "trigger", trigger, logging.AttrError, err)
		return
	}
	metricQueued.Add(1)
}

func (s *Service) delay(trigger models.QualityTrigger) time.Duration {
	switch trigger {
	case models.QualityTriggerReady:
		return s.cfg.ReadyDelay
	case models.QualityTriggerVersion:
		return s.cfg.VersionDelay
	}
	return 0
}

// editable reports whether a post in status is still being written.
func editable(status models.PostStatus) bool {
	switch status {
	case models.PostStatusDraft, models.PostStatusReadyForPublish,
		models.PostStatusFailed, models.PostStatusNotPublished:
		return true
	}
	return false
}

// Sweep claims the due assessments and runs them. ctx is a system context;
// each run gets its row's tenant.
func (s *Service) Sweep(ctx context.Context) error {
	if s == nil {
		return nil
	}
	rp := s.runner.Load()
	if rp == nil {
		return nil
	}
	run := *rp
	rows, err := s.queue.Claim(ctx, s.now(), sweepBatch, claimLease)
	if err != nil || len(rows) == 0 {
		return err
	}

	sem := make(chan struct{}, sweepConcurrency)
	var wg sync.WaitGroup
	errs := make([]error, len(rows))
	for i, q := range rows {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			errs[i] = s.runOne(ctx, run, q)
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// runOne runs one claimed assessment and settles its row: finished unless the
// failure is retryable and attempts remain.
func (s *Service) runOne(ctx context.Context, run Runner, q repository.QueuedAssessment) error {
	tctx := tenantctx.With(ctx, q.TenantID)
	outcome, err := s.attempt(tctx, run, q)
	log := slog.With(logging.AttrComponent, "qualityauto", "post_id", q.PostID, "trigger", q.Trigger)
	switch outcome {
	case Done:
		metricRun.Add(1)
	case Skipped:
		metricSkipped.Add(string(q.Trigger), 1)
		log.InfoContext(ctx, "automatic assessment skipped", logging.AttrError, err)
	case Failed:
		metricFailed.Add(1)
		log.WarnContext(ctx, "automatic assessment failed", "attempt", q.Attempts, logging.AttrError, err)
		if q.Attempts < maxAttempts {
			return s.queue.Retry(ctx, q, s.now().Add(retryAfter))
		}
	}
	return s.queue.Finish(ctx, q)
}

// attempt runs the assessment unless the post's tenant is no longer active.
func (s *Service) attempt(ctx context.Context, run Runner, q repository.QueuedAssessment) (Outcome, error) {
	if s.tenants != nil {
		st, err := s.tenants.GetStatus(ctx, q.TenantID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return Skipped, errors.New("tenant not found")
		case err != nil:
			return Failed, err
		case st != models.TenantStatusActive:
			return Skipped, errors.New("tenant " + st)
		}
	}
	return run(ctx, q.PostID, q.Trigger)
}
