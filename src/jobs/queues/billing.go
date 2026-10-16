package queues

import (
	"context"
	"database/sql"
	"time"
)

// BillingJobs is the billing usecase surface ogen's maintenance pass drives.
// *billing.Service implements it; the jobs that call Lemon Squeezy run as
// steps (billing_steps.go).
type BillingJobs interface {
	// SweepWebhookEvents prunes the webhook log.
	SweepWebhookEvents(ctx context.Context) error
}

// BillingReconcileQueue is the periodic subscription re-sync. Lemon Squeezy
// retries a webhook only three times, so the mirror cannot rely on webhooks
// alone. river-service schedules it.
const BillingReconcileQueue = "billing_reconcile"

// BillingWebhookSweepQueue prunes the billing webhook log daily.
// river-service schedules it; the pass runs in ogen.
const BillingWebhookSweepQueue = "billing_webhook_events_sweep"

// BillingScheduledChangeQueue advances a scheduled downgrade: at
// effective_at - 1h it switches the provider to the cheaper variant, at
// effective_at it moves the tier.
const BillingScheduledChangeQueue = "billing_apply_scheduled_change"

// BillingScheduledChangeTask names the change; the usecase decides which step
// is due from the row's status.
type BillingScheduledChangeTask struct {
	ChangeID string `json:"change_id"`
}

// Kind implements JobArgs.
func (BillingScheduledChangeTask) Kind() string { return BillingScheduledChangeQueue }

// EnqueueScheduledChangeStepTx schedules a step of a plan change at a time,
// inside the transaction that wrote the change.
func (e *Enqueuer) EnqueueScheduledChangeStepTx(ctx context.Context, tx *sql.Tx, changeID string, at time.Time) error {
	return e.insertTx(ctx, tx, BillingScheduledChangeTask{ChangeID: changeID}, &EnqueueOpts{ScheduledAt: at})
}
