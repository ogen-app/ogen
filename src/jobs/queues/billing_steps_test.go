package queues_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

// fakeBillingSteps scripts the billing usecase's step surface.
type fakeBillingSteps struct {
	plan     billingdom.ReconcilePlan
	next     *billingdom.PlanSwitch
	revert   *billingdom.PlanSwitch
	pushed   []error
	reverted []error
	resynced map[string]string
}

func (f *fakeBillingSteps) PlanReconcile(context.Context) (billingdom.ReconcilePlan, error) {
	return f.plan, nil
}

func (f *fakeBillingSteps) ApplyResync(_ context.Context, id string, snap billingdom.Subscription, callErr error) error {
	if f.resynced == nil {
		f.resynced = map[string]string{}
	}
	if callErr != nil {
		return callErr
	}
	f.resynced[id] = snap.Status
	return nil
}

func (f *fakeBillingSteps) NextScheduledChange(context.Context, string) (*billingdom.PlanSwitch, error) {
	return f.next, nil
}

func (f *fakeBillingSteps) ScheduledChangePushed(_ context.Context, _ string, _ billingdom.Subscription, callErr error, _ bool) (*billingdom.PlanSwitch, error) {
	f.pushed = append(f.pushed, callErr)
	return f.revert, nil
}

func (f *fakeBillingSteps) ScheduledChangeReverted(_ context.Context, _ billingdom.Subscription, callErr error) error {
	f.reverted = append(f.reverted, callErr)
	return nil
}

func subscriptionBody(id, variant, status string) []byte {
	return []byte(`{"data":{"type":"subscriptions","id":"` + id + `","attributes":{"status":"` + status + `","variant_id":` + variant + `}}}`)
}

// driveBilling steps a billing job, answering each Lemon Squeezy call with
// answer, and returns the last output and the calls made.
func driveBilling(t *testing.T, steps *queues.JobSteps, kind string, args string, answer func(queues.HTTPCall) queues.StepResult) (queues.StepOutput, []string) {
	t.Helper()
	in := queues.StepInput{Kind: kind, Args: []byte(args), Attempt: 1, MaxAttempts: 5}
	var calls []string
	for range 10 {
		out, err := steps.Step(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if out.HTTP == nil {
			return out, calls
		}
		if out.HTTP.Upstream != queues.UpstreamLemonSqueezy {
			t.Fatalf("call to %q, want lemonsqueezy", out.HTTP.Upstream)
		}
		calls = append(calls, out.HTTP.Method+" "+out.HTTP.Path)
		res := answer(*out.HTTP)
		in.State, in.Result = out.State, &res
	}
	t.Fatal("job did not finish")
	return queues.StepOutput{}, nil
}

func TestScheduledChangeStepsPushThroughRiverService(t *testing.T) {
	svc := &fakeBillingSteps{next: &billingdom.PlanSwitch{SubscriptionID: "42", VariantID: "7"}}
	steps := queues.NewJobSteps(queues.Deps{BillingSteps: svc})
	out, calls := driveBilling(t, steps, queues.BillingScheduledChangeQueue, `{"change_id":"ch1"}`, func(c queues.HTTPCall) queues.StepResult {
		var body struct {
			Data struct {
				Attributes map[string]any `json:"attributes"`
			} `json:"data"`
		}
		if err := json.Unmarshal(c.Body, &body); err != nil || body.Data.Attributes["disable_prorations"] != true {
			t.Errorf("push body = %s, want no proration", c.Body)
		}
		return queues.StepResult{Status: 200, Body: subscriptionBody("42", "7", "active")}
	})
	if out.Done == nil || out.Done.Outcome != "pushed" || len(calls) != 1 || calls[0] != http.MethodPatch+" /subscriptions/42" {
		t.Fatalf("out = %+v, calls = %v", out, calls)
	}
	if len(svc.pushed) != 1 || svc.pushed[0] != nil {
		t.Fatalf("pushed outcomes = %v", svc.pushed)
	}
}

func TestScheduledChangeStepsRevertAMissedPush(t *testing.T) {
	svc := &fakeBillingSteps{
		next:   &billingdom.PlanSwitch{SubscriptionID: "42", VariantID: "7"},
		revert: &billingdom.PlanSwitch{SubscriptionID: "42", VariantID: "9"},
	}
	steps := queues.NewJobSteps(queues.Deps{BillingSteps: svc})
	out, calls := driveBilling(t, steps, queues.BillingScheduledChangeQueue, `{"change_id":"ch1"}`, func(queues.HTTPCall) queues.StepResult {
		return queues.StepResult{Status: 200, Body: subscriptionBody("42", "9", "active")}
	})
	if out.Done == nil || out.Done.Outcome != "reverted" || len(calls) != 2 || len(svc.reverted) != 1 || svc.reverted[0] != nil {
		t.Fatalf("out = %+v, calls = %v, reverted = %v", out, calls, svc.reverted)
	}
}

func TestScheduledChangeStepsReportAMissingKey(t *testing.T) {
	svc := &fakeBillingSteps{next: &billingdom.PlanSwitch{SubscriptionID: "42", VariantID: "7"}}
	steps := queues.NewJobSteps(queues.Deps{BillingSteps: svc})
	driveBilling(t, steps, queues.BillingScheduledChangeQueue, `{"change_id":"ch1"}`, func(queues.HTTPCall) queues.StepResult {
		return queues.StepResult{CredentialMissing: true}
	})
	if len(svc.pushed) != 1 || !errors.Is(svc.pushed[0], billingdom.ErrNotConfigured) {
		t.Fatalf("pushed outcomes = %v, want ErrNotConfigured", svc.pushed)
	}
}

func TestReconcileStepsFanOutOneReadPerSubscription(t *testing.T) {
	svc := &fakeBillingSteps{plan: billingdom.ReconcilePlan{Subscriptions: []string{"41", "42"}}}
	steps := queues.NewJobSteps(queues.Deps{BillingSteps: svc})
	out, err := steps.Step(t.Context(), queues.StepInput{Kind: queues.BillingReconcileQueue})
	if err != nil || out.Done == nil || len(out.Done.Enqueue) != 2 || out.Done.Enqueue[1].Kind != queues.BillingResyncKind {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	child := out.Done.Enqueue[1]
	out, calls := driveBilling(t, steps, child.Kind, string(child.Args), func(queues.HTTPCall) queues.StepResult {
		return queues.StepResult{Status: 200, Body: subscriptionBody("42", "7", "past_due")}
	})
	if out.Done == nil || len(calls) != 1 || calls[0] != http.MethodGet+" /subscriptions/42" || svc.resynced["42"] != "past_due" {
		t.Fatalf("out = %+v, calls = %v, resynced = %v", out, calls, svc.resynced)
	}

	// A read that failed retries the child.
	out, _ = driveBilling(t, steps, child.Kind, string(child.Args), func(queues.HTTPCall) queues.StepResult {
		return queues.StepResult{Status: 503}
	})
	if out.Retry == nil {
		t.Fatalf("503: %+v, want a retry", out)
	}
}
