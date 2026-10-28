package queues_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

type fakeBillingPayments struct {
	off      bool
	recorded []billingdom.Payment
}

func (f *fakeBillingPayments) PaymentsStore() (string, bool) { return "77", !f.off }

func (f *fakeBillingPayments) RecordPayments(_ context.Context, ps []billingdom.Payment) error {
	f.recorded = append(f.recorded, ps...)
	return nil
}

func paymentsPage(typ, id string, lastPage int) []byte {
	return fmt.Appendf(nil, `{"meta":{"page":{"lastPage":%d}},"data":[{"type":%q,"id":%q,"attributes":
		{"status":"paid","currency":"USD","total":2900,"total_usd":2900,"created_at":"2026-10-01T00:00:00Z"}}]}`, lastPage, typ, id)
}

func TestSyncPaymentsPagesOrdersThenInvoices(t *testing.T) {
	svc := &fakeBillingPayments{}
	steps := queues.NewJobSteps(queues.Deps{BillingPayments: svc})
	out, calls := driveBilling(t, steps, queues.BillingSyncPaymentsKind, `{}`, func(c queues.HTTPCall) queues.StepResult {
		if c.Query.Get("filter[store_id]") != "77" || c.Query.Get("page[size]") != "100" {
			t.Errorf("query = %v", c.Query)
		}
		page, _ := strconv.Atoi(c.Query.Get("page[number]"))
		if c.Path == "/orders" {
			return queues.StepResult{Status: 200, Body: paymentsPage("orders", "o"+strconv.Itoa(page), 2)}
		}
		return queues.StepResult{Status: 200, Body: paymentsPage("subscription-invoices", "i"+strconv.Itoa(page), 1)}
	})
	want := []string{"GET /orders", "GET /orders", "GET /subscription-invoices"}
	if out.Done == nil || out.Done.Outcome != "synced" || fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("out = %+v, calls = %v", out, calls)
	}
	var ids []string
	for _, p := range svc.recorded {
		ids = append(ids, p.Source+":"+p.ProviderID)
	}
	if fmt.Sprint(ids) != "[order:o1 order:o2 subscription_invoice:i1]" {
		t.Fatalf("recorded = %v", ids)
	}
}

func TestSyncPaymentsSkips(t *testing.T) {
	steps := queues.NewJobSteps(queues.Deps{BillingPayments: &fakeBillingPayments{off: true}})
	out, err := steps.Step(t.Context(), queues.StepInput{Kind: queues.BillingSyncPaymentsKind, Args: []byte(`{}`)})
	if err != nil || out.Done == nil || out.Done.Outcome != "skipped_billing_off" {
		t.Fatalf("billing off: %+v %v", out, err)
	}

	steps = queues.NewJobSteps(queues.Deps{BillingPayments: &fakeBillingPayments{}})
	out, _ = driveBilling(t, steps, queues.BillingSyncPaymentsKind, `{}`, func(queues.HTTPCall) queues.StepResult {
		return queues.StepResult{CredentialMissing: true}
	})
	if out.Done == nil || out.Done.Outcome != "skipped_not_configured" {
		t.Fatalf("no key: %+v", out)
	}

	out, _ = driveBilling(t, steps, queues.BillingSyncPaymentsKind, `{}`, func(queues.HTTPCall) queues.StepResult {
		return queues.StepResult{Status: 503}
	})
	if out.Retry == nil {
		t.Fatalf("provider down: %+v, want a retry", out)
	}
}
