package queues_test

import (
	"context"
	"net/http"
	"testing"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/infra/billing/polar"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

// sandboxRemote is a Polar client in sandbox mode, planning calls only.
func sandboxRemote() *polar.Client {
	return polar.New(polar.Options{OrganizationID: "org_1", TestMode: true, APIVersion: "2026-10"})
}

// fakeBillingSteps scripts the billing usecase's step surface.
type fakeBillingSteps struct {
	plan     billingdom.ReconcilePlan
	resynced map[string]string
	off      bool
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

func (f *fakeBillingSteps) Remote() billingdom.Remote {
	if f.off {
		return nil
	}
	return sandboxRemote()
}

func subscriptionBody(id, product, status string) []byte {
	return []byte(`{"id":"` + id + `","status":"` + status + `","product_id":"` + product + `","currency":"eur","created_at":"2026-11-01T00:00:00Z"}`)
}

// driveBilling steps a billing job, answering each outside call with answer,
// and returns the last output and the calls made.
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
		switch {
		case out.HTTP.URL != "":
			calls = append(calls, out.HTTP.Method+" "+out.HTTP.URL)
		case out.HTTP.Upstream != polar.UpstreamSandbox:
			t.Fatalf("call to %q, want %s", out.HTTP.Upstream, polar.UpstreamSandbox)
		default:
			if out.HTTP.Headers["Polar-Version"] != "2026-10" {
				t.Fatalf("headers = %v, want the pinned API version", out.HTTP.Headers)
			}
			calls = append(calls, out.HTTP.Method+" "+out.HTTP.Path)
		}
		res := answer(*out.HTTP)
		in.State, in.Result = out.State, &res
	}
	t.Fatal("job did not finish")
	return queues.StepOutput{}, nil
}

func TestScheduledChangeJobsDrainAsNoOps(t *testing.T) {
	steps := queues.NewJobSteps(queues.Deps{BillingSteps: &fakeBillingSteps{}})
	out, calls := driveBilling(t, steps, queues.BillingScheduledChangeQueue, `{"change_id":"ch1"}`, func(queues.HTTPCall) queues.StepResult {
		return queues.StepResult{}
	})
	if out.Done == nil || out.Done.Outcome != "retired" || len(calls) != 0 {
		t.Fatalf("out = %+v, calls = %v", out, calls)
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
		return queues.StepResult{Status: 200, Body: subscriptionBody("42", "prod_1", "past_due")}
	})
	if out.Done == nil || len(calls) != 1 || calls[0] != http.MethodGet+" /subscriptions/42" || svc.resynced["42"] != "past_due" {
		t.Fatalf("out = %+v, calls = %v, resynced = %v", out, calls, svc.resynced)
	}

	// A read that failed retries the child; a missing token is reported.
	out, _ = driveBilling(t, steps, child.Kind, string(child.Args), func(queues.HTTPCall) queues.StepResult {
		return queues.StepResult{Status: 503}
	})
	if out.Retry == nil {
		t.Fatalf("503: %+v, want a retry", out)
	}
	out, _ = driveBilling(t, steps, child.Kind, string(child.Args), func(queues.HTTPCall) queues.StepResult {
		return queues.StepResult{CredentialMissing: true}
	})
	if out.Retry == nil {
		t.Fatalf("no token: %+v, want a retry", out)
	}

	// With billing off the child does nothing.
	steps = queues.NewJobSteps(queues.Deps{BillingSteps: &fakeBillingSteps{off: true}})
	if out, _ := driveBilling(t, steps, child.Kind, string(child.Args), nil); out.Done == nil || out.Done.Outcome != "skipped_billing_off" {
		t.Fatalf("billing off: %+v", out)
	}
}
