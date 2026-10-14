package queues

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

// BillingJobs is the billing usecase surface the billing workers drive. The
// workers stay thin: all billing logic lives in the usecase.
type BillingJobs interface {
	// Reconcile re-reads live subscriptions from the provider, re-drives due
	// scheduled changes and catches missed unpaid-trial expiries.
	Reconcile(ctx context.Context) error
	// SweepWebhookEvents prunes the webhook log.
	SweepWebhookEvents(ctx context.Context) error
	// RunScheduledChange advances one scheduled plan change.
	RunScheduledChange(ctx context.Context, changeID string) error
}

// BillingReconcileQueue is the periodic subscription re-sync. Lemon Squeezy
// retries a webhook only three times, so the mirror cannot rely on webhooks
// alone.
const BillingReconcileQueue = "billing_reconcile"

// BillingReconcileTask is the periodic marker.
type BillingReconcileTask struct{}

// Kind implements river.JobArgs.
func (BillingReconcileTask) Kind() string { return BillingReconcileQueue }

// InsertOpts dedupes overlapping ticks.
func (BillingReconcileTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

// BillingReconcileProcessor runs one reconcile pass.
type BillingReconcileProcessor struct {
	river.WorkerDefaults[BillingReconcileTask]
	Billing BillingJobs
}

// Work is the River entrypoint.
func (p *BillingReconcileProcessor) Work(ctx context.Context, job *river.Job[BillingReconcileTask]) error {
	if p.Billing == nil {
		return nil
	}
	return p.Billing.Reconcile(WithJobRequestID(ctx, job.JobRow))
}

// Timeout bounds one pass; it reads every live subscription.
func (p *BillingReconcileProcessor) Timeout(*river.Job[BillingReconcileTask]) time.Duration {
	return 10 * time.Minute
}

// BillingWebhookSweepQueue prunes the billing webhook log daily.
const BillingWebhookSweepQueue = "billing_webhook_events_sweep"

// BillingWebhookSweepTask is the periodic marker.
type BillingWebhookSweepTask struct{}

// Kind implements river.JobArgs.
func (BillingWebhookSweepTask) Kind() string { return BillingWebhookSweepQueue }

// InsertOpts dedupes overlapping ticks.
func (BillingWebhookSweepTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3, UniqueOpts: periodicUniqueOpts()}
}

// BillingWebhookSweepProcessor prunes the webhook log.
type BillingWebhookSweepProcessor struct {
	river.WorkerDefaults[BillingWebhookSweepTask]
	Billing BillingJobs
}

// Work is the River entrypoint.
func (p *BillingWebhookSweepProcessor) Work(ctx context.Context, _ *river.Job[BillingWebhookSweepTask]) error {
	if p.Billing == nil {
		return nil
	}
	return p.Billing.SweepWebhookEvents(ctx)
}

// BillingScheduledChangeQueue advances a scheduled downgrade: at
// effective_at - 1h it switches the provider to the cheaper variant, at
// effective_at it moves the tier.
const BillingScheduledChangeQueue = "billing_apply_scheduled_change"

// BillingScheduledChangeTask names the change; the usecase decides which step
// is due from the row's status.
type BillingScheduledChangeTask struct {
	ChangeID string `json:"change_id"`
}

// Kind implements river.JobArgs.
func (BillingScheduledChangeTask) Kind() string { return BillingScheduledChangeQueue }

// InsertOpts retries provider failures; the final failure marks the change
// failed (the usecase does that on the last attempt).
func (BillingScheduledChangeTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 5}
}

// BillingScheduledChangeProcessor runs one step of a scheduled change.
type BillingScheduledChangeProcessor struct {
	river.WorkerDefaults[BillingScheduledChangeTask]
	Billing BillingJobs
}

// Work is the River entrypoint.
func (p *BillingScheduledChangeProcessor) Work(ctx context.Context, job *river.Job[BillingScheduledChangeTask]) error {
	if p.Billing == nil {
		return nil
	}
	ctx = WithJobRequestID(ctx, job.JobRow)
	err := p.Billing.RunScheduledChange(WithLastAttempt(ctx, job.Attempt >= job.MaxAttempts), job.Args.ChangeID)
	if err != nil {
		slog.WarnContext(ctx, "scheduled plan change step failed", logging.AttrComponent, "jobs.billing", "change_id", job.Args.ChangeID, "attempt", job.Attempt, logging.AttrError, err)
	}
	return err
}

type lastAttemptKey struct{}

// WithLastAttempt marks ctx as a job's final attempt, so the usecase can record
// a terminal failure instead of leaving the row for a retry that never comes.
func WithLastAttempt(ctx context.Context, last bool) context.Context {
	return context.WithValue(ctx, lastAttemptKey{}, last)
}

// IsLastAttempt reports whether ctx is a job's final attempt.
func IsLastAttempt(ctx context.Context) bool {
	last, _ := ctx.Value(lastAttemptKey{}).(bool)
	return last
}

// EnqueueScheduledChangeStepTx schedules a step of a plan change at a time,
// inside the transaction that wrote the change.
func (e *Enqueuer) EnqueueScheduledChangeStepTx(ctx context.Context, tx *sql.Tx, changeID string, at time.Time) error {
	return e.insertTx(ctx, tx, BillingScheduledChangeTask{ChangeID: changeID}, &river.InsertOpts{ScheduledAt: at})
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &BillingReconcileProcessor{Billing: d.Billing})
		river.AddWorker(w, &BillingWebhookSweepProcessor{Billing: d.Billing})
		river.AddWorker(w, &BillingScheduledChangeProcessor{Billing: d.Billing})
	})
}
