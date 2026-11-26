package queues

import "context"

// BillingJobs is the billing usecase surface ogen's maintenance pass drives.
// *billing.Service implements it; the jobs that call the provider run as
// steps (billing_steps.go).
type BillingJobs interface {
	// SweepWebhookEvents prunes the webhook log.
	SweepWebhookEvents(ctx context.Context) error
}

// BillingReconcileQueue is the periodic subscription re-sync. Webhooks are
// retried for a while and then dropped, and their order is not guaranteed,
// so the mirror cannot rely on webhooks alone. river-service schedules it.
const BillingReconcileQueue = "billing_reconcile"

// BillingWebhookSweepQueue prunes the billing webhook log daily.
// river-service schedules it; the pass runs in ogen.
const BillingWebhookSweepQueue = "billing_webhook_events_sweep"

// BillingScheduledChangeQueue advanced a Lemon Squeezy downgrade in two timed
// steps. The provider now holds a downgrade itself, so nothing enqueues it;
// jobs still queued drain as no-ops.
const BillingScheduledChangeQueue = "billing_apply_scheduled_change"

// BillingScheduledChangeTask is the queued job's arguments, kept so jobs
// queued before still decode.
type BillingScheduledChangeTask struct {
	ChangeID string `json:"change_id"`
}

// Kind implements JobArgs.
func (BillingScheduledChangeTask) Kind() string { return BillingScheduledChangeQueue }
