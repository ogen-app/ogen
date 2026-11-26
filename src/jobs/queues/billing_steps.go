package queues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
)

// Billing under river-service. The provider calls are made by river-service
// with its leased token; ogen plans and decodes each one with the ordinary
// client (billingdom.Remote). A reconcile tick fans out one
// billing_resync_subscription child per subscription to re-read, so each step
// is one call and the tick fits any drain.

// BillingResyncKind re-reads one subscription for the reconcile sweep. Only
// river-service runs it, as a child of billing_reconcile.
const BillingResyncKind = "billing_resync_subscription"

// BillingSteps is the billing usecase surface the steps drive.
// *billing.Service implements it.
type BillingSteps interface {
	PlanReconcile(ctx context.Context) (billingdom.ReconcilePlan, error)
	ApplyResync(ctx context.Context, providerSubID string, snap billingdom.Subscription, callErr error) error
	// Remote plans the provider's calls; nil while billing is off.
	Remote() billingdom.Remote
}

type billingSteps struct {
	svc BillingSteps
}

// registerBilling adds the billing jobs.
func (s *JobSteps) registerBilling() {
	if s.deps.BillingPayments != nil {
		s.kinds[BillingSyncPaymentsKind] = billingPaymentsSteps{svc: s.deps.BillingPayments}.syncPayments
	}
	if s.deps.BillingSteps == nil {
		return
	}
	b := billingSteps{svc: s.deps.BillingSteps}
	s.kinds[BillingReconcileQueue] = b.reconcile
	s.kinds[BillingResyncKind] = b.resync
	s.kinds[BillingScheduledChangeQueue] = retiredScheduledChange
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
	remote := b.svc.Remote()
	if remote == nil {
		return done("skipped_billing_off")
	}
	id := args.ProviderSubscriptionID
	read := func(p billingdom.Provider) (billingdom.Subscription, error) { return p.GetSubscription(ctx, id) }
	if in.Result == nil {
		return planProviderCall(remote, struct{}{}, read)
	}
	snap, callErr := providerResult(remote, in.Result, read)
	if err := b.svc.ApplyResync(ctx, id, snap, callErr); err != nil {
		return retry("billing_resync_subscription", err)
	}
	return done("resynced")
}

// retiredScheduledChange drains billing_apply_scheduled_change jobs queued
// before the provider held downgrades itself: there is nothing left to do.
func retiredScheduledChange(context.Context, StepInput) (StepOutput, error) {
	return done("retired")
}

// planProviderCall asks river-service to make the call op plans.
func planProviderCall[T any](remote billingdom.Remote, state any, op func(p billingdom.Provider) (T, error)) (StepOutput, error) {
	call, err := remote.Plan(func(p billingdom.Provider) error { _, err := op(p); return err })
	if err != nil {
		return StepOutput{}, err
	}
	return providerStep(remote.Upstream(), state, call)
}

// providerStep hands river-service a planned call, its query split off the
// path so river-service builds the URL with the base it owns.
func providerStep(upstream string, state any, call billingdom.Call) (StepOutput, error) {
	raw, err := encodeState(state)
	if err != nil {
		return StepOutput{}, err
	}
	path, rawQuery, _ := strings.Cut(call.Path, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return StepOutput{}, fmt.Errorf("billing: planned query: %w", err)
	}
	if len(query) == 0 {
		query = nil
	}
	return StepOutput{State: raw, HTTP: &HTTPCall{
		Upstream: upstream, Method: call.Method, Path: path, Query: query, Headers: call.Headers, Body: call.Body,
	}}, nil
}

// providerResult decodes river-service's result of op's call. No token in
// ogen reads as billing.ErrNotConfigured, as a direct call would.
func providerResult[T any](remote billingdom.Remote, r *StepResult, op func(p billingdom.Provider) (T, error)) (T, error) {
	var out T
	switch {
	case r == nil:
		return out, errors.New("no result")
	case r.CredentialMissing:
		return out, billingdom.ErrNotConfigured
	}
	err := remote.Decode(billingdom.CallResult{Status: r.Status, Body: r.Body, Err: r.Err}, func(p billingdom.Provider) error {
		var err error
		out, err = op(p)
		return err
	})
	return out, err
}
