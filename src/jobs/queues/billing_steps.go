package queues

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/infra/billing/lemonsqueezy"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// Billing under river-service. The Lemon Squeezy calls are made by
// river-service with its leased key; ogen plans and decodes each one with the
// ordinary client (lemonsqueezy.Plan / Decode). A reconcile tick fans out one
// billing_resync_subscription child per subscription to re-read, so each step
// is one call and the tick fits any drain; its due scheduled changes are
// queued as billing_apply_scheduled_change jobs.

// BillingResyncKind re-reads one subscription for the reconcile sweep. Only
// river-service runs it, as a child of billing_reconcile.
const BillingResyncKind = "billing_resync_subscription"

// BillingSteps is the billing usecase surface the steps drive.
// *billing.Service implements it.
type BillingSteps interface {
	PlanReconcile(ctx context.Context) (billingdom.ReconcilePlan, error)
	ApplyResync(ctx context.Context, providerSubID string, snap billingdom.Subscription, callErr error) error
	NextScheduledChange(ctx context.Context, changeID string) (*billingdom.PlanSwitch, error)
	ScheduledChangePushed(ctx context.Context, changeID string, snap billingdom.Subscription, callErr error, lastAttempt bool) (*billingdom.PlanSwitch, error)
	ScheduledChangeReverted(ctx context.Context, snap billingdom.Subscription, callErr error) error
}

type billingSteps struct {
	svc  BillingSteps
	jobs *Enqueuer
}

// registerBilling adds the billing jobs. ogen's own River workers call the
// provider directly over the same usecase pieces.
func (s *JobSteps) registerBilling() {
	if s.deps.BillingSteps == nil {
		return
	}
	b := billingSteps{svc: s.deps.BillingSteps, jobs: s.deps.Jobs}
	s.kinds[BillingReconcileQueue] = b.reconcile
	s.kinds[BillingResyncKind] = b.resync
	s.kinds[BillingScheduledChangeQueue] = b.scheduledChange
}

type resyncArgs struct {
	ProviderSubscriptionID string `json:"provider_subscription_id"`
}

// reconcile runs the provider-free part of a pass and fans out the rest.
func (b billingSteps) reconcile(ctx context.Context, _ StepInput) (StepOutput, error) {
	plan, err := b.svc.PlanReconcile(ctx)
	if err != nil {
		return retry("billing_reconcile", err)
	}
	for _, id := range plan.DueChanges {
		if err := b.jobs.enqueue(ctx, BillingScheduledChangeTask{ChangeID: id}, nil); err != nil {
			slog.WarnContext(ctx, "billing reconcile: could not queue a due scheduled change", logging.AttrComponent, "jobs.billing", "change_id", id, logging.AttrError, err)
		}
	}
	children := make([]ChildJob, 0, len(plan.Subscriptions))
	for _, id := range plan.Subscriptions {
		raw, err := json.Marshal(resyncArgs{ProviderSubscriptionID: id})
		if err != nil {
			return StepOutput{}, err
		}
		children = append(children, ChildJob{Kind: BillingResyncKind, Args: raw})
	}
	return done("planned", children...)
}

// resync re-reads one subscription and applies it.
func (b billingSteps) resync(ctx context.Context, in StepInput) (StepOutput, error) {
	var args resyncArgs
	if err := json.Unmarshal(in.Args, &args); err != nil || args.ProviderSubscriptionID == "" {
		return done("skipped_bad_args")
	}
	id := args.ProviderSubscriptionID
	read := func(c *lemonsqueezy.Client) (billingdom.Subscription, error) { return c.GetSubscription(ctx, id) }
	if in.Result == nil {
		return nextLemonSqueezy(billingState{Phase: billingPhaseRead}, read)
	}
	snap, callErr := lemonSqueezyResult(in.Result, read)
	if err := b.svc.ApplyResync(ctx, id, snap, callErr); err != nil {
		return retry("billing_resync_subscription", err)
	}
	return done("resynced")
}

type billingState struct {
	Phase  string                 `json:"phase"`
	Switch *billingdom.PlanSwitch `json:"switch,omitempty"`
}

const (
	billingPhaseRead   = "read"
	billingPhasePush   = "push"
	billingPhaseRevert = "revert"
)

// scheduledChange advances one scheduled plan change: the steps that need no
// provider call run here, the provider switch (and, for a push that landed
// too late, its revert) through river-service.
func (b billingSteps) scheduledChange(ctx context.Context, in StepInput) (StepOutput, error) {
	var task BillingScheduledChangeTask
	if err := json.Unmarshal(in.Args, &task); err != nil || task.ChangeID == "" {
		return done("skipped_bad_args")
	}
	var st billingState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	switch st.Phase {
	case "":
		sw, err := b.svc.NextScheduledChange(ctx, task.ChangeID)
		if err != nil {
			return retry("billing_apply_scheduled_change", err)
		}
		if sw == nil {
			return done("settled")
		}
		return planSwitch(ctx, billingPhasePush, sw)
	case billingPhasePush:
		snap, callErr := lemonSqueezyResult(in.Result, changePlan(ctx, st.Switch))
		revert, err := b.svc.ScheduledChangePushed(ctx, task.ChangeID, snap, callErr, in.lastAttempt())
		if err != nil {
			return retry("billing_apply_scheduled_change", err)
		}
		if revert == nil {
			return done("pushed")
		}
		return planSwitch(ctx, billingPhaseRevert, revert)
	case billingPhaseRevert:
		snap, callErr := lemonSqueezyResult(in.Result, changePlan(ctx, st.Switch))
		if err := b.svc.ScheduledChangeReverted(ctx, snap, callErr); err != nil {
			return retry("billing_apply_scheduled_change", err)
		}
		return done("reverted")
	default:
		return done("skipped_bad_state")
	}
}

func changePlan(ctx context.Context, sw *billingdom.PlanSwitch) func(c *lemonsqueezy.Client) (billingdom.Subscription, error) {
	return func(c *lemonsqueezy.Client) (billingdom.Subscription, error) {
		if sw == nil {
			return billingdom.Subscription{}, errors.New("no plan switch in the job state")
		}
		return c.ChangePlan(ctx, sw.SubscriptionID, sw.VariantID, sw.Opts())
	}
}

func planSwitch(ctx context.Context, phase string, sw *billingdom.PlanSwitch) (StepOutput, error) {
	return nextLemonSqueezy(billingState{Phase: phase, Switch: sw}, changePlan(ctx, sw))
}

// nextLemonSqueezy asks river-service to make the call op plans.
func nextLemonSqueezy[T any](state billingState, op func(c *lemonsqueezy.Client) (T, error)) (StepOutput, error) {
	call, err := lemonsqueezy.Plan(func(c *lemonsqueezy.Client) error { _, err := op(c); return err })
	if err != nil {
		return StepOutput{}, err
	}
	raw, err := encodeState(state)
	if err != nil {
		return StepOutput{}, err
	}
	return StepOutput{State: raw, HTTP: &HTTPCall{
		Upstream: UpstreamLemonSqueezy, Method: call.Method, Path: call.Path, Headers: call.Headers, Body: call.Body,
	}}, nil
}

// lemonSqueezyResult decodes river-service's result of op's call. No key in
// ogen reads as billing.ErrNotConfigured, as a direct call would.
func lemonSqueezyResult[T any](r *StepResult, op func(c *lemonsqueezy.Client) (T, error)) (T, error) {
	var out T
	switch {
	case r == nil:
		return out, errors.New("no result")
	case r.CredentialMissing:
		return out, billingdom.ErrNotConfigured
	}
	err := lemonsqueezy.Decode(lemonsqueezy.Result{Status: r.Status, Body: r.Body, Err: r.Err}, func(c *lemonsqueezy.Client) error {
		var err error
		out, err = op(c)
		return err
	})
	return out, err
}
