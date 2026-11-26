package queues_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/infra/billing/ecb"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

type fakeBillingPayments struct {
	off      bool
	recorded []billingdom.Payment
	rates    []billingdom.FXRate
}

func (f *fakeBillingPayments) Remote() billingdom.Remote {
	if f.off {
		return nil
	}
	return sandboxRemote()
}

func (f *fakeBillingPayments) PaymentSources() []string {
	if f.off {
		return nil
	}
	return []string{billingdom.PaymentSourceOrder, billingdom.PaymentSourceRefund}
}

func (f *fakeBillingPayments) RecordPayments(_ context.Context, ps []billingdom.Payment) error {
	f.recorded = append(f.recorded, ps...)
	return nil
}

func (f *fakeBillingPayments) RecordFXRates(_ context.Context, rates []billingdom.FXRate) error {
	f.rates = append(f.rates, rates...)
	return nil
}

func ordersPage(id string, maxPage int) []byte {
	return fmt.Appendf(nil, `{"items":[{"id":%q,"status":"paid","currency":"eur","total_amount":2900,"subtotal_amount":2900,
		"billing_reason":"subscription_cycle","created_at":"2026-10-01T00:00:00Z"}],"pagination":{"total_count":2,"max_page":%d}}`, id, maxPage)
}

func refundsPage(id string) []byte {
	return fmt.Appendf(nil, `{"items":[{"id":%q,"status":"succeeded","currency":"eur","amount":1000,"tax_amount":0,
		"order_id":"o1","created_at":"2026-10-02T00:00:00Z"}],"pagination":{"total_count":1,"max_page":1}}`, id)
}

const ratesFile = `<Envelope><Cube><Cube time="2026-10-01"><Cube currency="USD" rate="1.1"/></Cube></Cube></Envelope>`

func TestSyncPaymentsRefreshesRatesThenPagesOrdersAndRefunds(t *testing.T) {
	svc := &fakeBillingPayments{}
	steps := queues.NewJobSteps(queues.Deps{BillingPayments: svc})
	out, calls := driveBilling(t, steps, queues.BillingSyncPaymentsKind, `{}`, func(c queues.HTTPCall) queues.StepResult {
		if c.URL != "" {
			return queues.StepResult{Status: 200, Body: []byte(ratesFile)}
		}
		if c.Query.Get("organization_id") != "org_1" || c.Query.Get("limit") != "100" {
			t.Errorf("query = %v", c.Query)
		}
		page, _ := strconv.Atoi(c.Query.Get("page"))
		if c.Path == "/orders/" {
			return queues.StepResult{Status: 200, Body: ordersPage("o"+strconv.Itoa(page), 2)}
		}
		return queues.StepResult{Status: 200, Body: refundsPage("r" + strconv.Itoa(page))}
	})
	want := []string{"GET " + ecb.HistoryURL, "GET /orders/", "GET /orders/", "GET /refunds/"}
	if out.Done == nil || out.Done.Outcome != "synced" || fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("out = %+v, calls = %v", out, calls)
	}
	var ids []string
	for _, p := range svc.recorded {
		ids = append(ids, p.Source+":"+p.ProviderID)
	}
	if fmt.Sprint(ids) != "[order:o1 order:o2 refund:r1]" {
		t.Fatalf("recorded = %v", ids)
	}
	if len(svc.rates) != 1 || svc.rates[0].Currency != "USD" {
		t.Fatalf("rates = %+v", svc.rates)
	}
}

func TestSyncPaymentsGoesOnWithoutRates(t *testing.T) {
	svc := &fakeBillingPayments{}
	steps := queues.NewJobSteps(queues.Deps{BillingPayments: svc})
	out, _ := driveBilling(t, steps, queues.BillingSyncPaymentsKind, `{}`, func(c queues.HTTPCall) queues.StepResult {
		if c.URL != "" {
			return queues.StepResult{Status: http.StatusServiceUnavailable}
		}
		if c.Path == "/orders/" {
			return queues.StepResult{Status: 200, Body: ordersPage("o1", 1)}
		}
		return queues.StepResult{Status: 200, Body: refundsPage("r1")}
	})
	if out.Done == nil || out.Done.Outcome != "synced" || len(svc.recorded) != 2 || len(svc.rates) != 0 {
		t.Fatalf("out = %+v, recorded %d, rates %d", out, len(svc.recorded), len(svc.rates))
	}
}

func TestSyncPaymentsSkips(t *testing.T) {
	steps := queues.NewJobSteps(queues.Deps{BillingPayments: &fakeBillingPayments{off: true}})
	out, err := steps.Step(t.Context(), queues.StepInput{Kind: queues.BillingSyncPaymentsKind, Args: []byte(`{}`)})
	if err != nil || out.Done == nil || out.Done.Outcome != "skipped_billing_off" {
		t.Fatalf("billing off: %+v %v", out, err)
	}

	steps = queues.NewJobSteps(queues.Deps{BillingPayments: &fakeBillingPayments{}})
	out, _ = driveBilling(t, steps, queues.BillingSyncPaymentsKind, `{}`, func(c queues.HTTPCall) queues.StepResult {
		if c.URL != "" {
			return queues.StepResult{Status: 200, Body: []byte(ratesFile)}
		}
		return queues.StepResult{CredentialMissing: true}
	})
	if out.Done == nil || out.Done.Outcome != "skipped_not_configured" {
		t.Fatalf("no token: %+v", out)
	}

	out, _ = driveBilling(t, steps, queues.BillingSyncPaymentsKind, `{}`, func(c queues.HTTPCall) queues.StepResult {
		if c.URL != "" {
			return queues.StepResult{Status: 200, Body: []byte(ratesFile)}
		}
		return queues.StepResult{Status: 503}
	})
	if out.Retry == nil {
		t.Fatalf("provider down: %+v, want a retry", out)
	}
}
