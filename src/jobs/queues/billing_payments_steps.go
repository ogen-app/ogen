package queues

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/infra/billing/ecb"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// BillingSyncPaymentsKind refreshes the exchange rates, then pages through
// the provider's whole payment history, source by source (Polar: orders,
// then refunds), and upserts every page. Run once it backfills the payments
// that predate the webhooks; run on a schedule it picks up refunds whose
// webhook was lost and converts payments whose rate was not known yet.
// river-service schedules it and makes each call with the leased token of the
// current billing mode.
const BillingSyncPaymentsKind = "billing_sync_payments"

// BillingPayments is the billing usecase surface the payments sync drives.
// *billing.Service implements it.
type BillingPayments interface {
	// Remote plans the provider's calls; nil while billing is off.
	Remote() billingdom.Remote
	// PaymentSources lists the provider's payment histories, in order.
	PaymentSources() []string
	RecordPayments(ctx context.Context, payments []billingdom.Payment) error
	RecordFXRates(ctx context.Context, rates []billingdom.FXRate) error
}

// paymentsSyncState is where the sync is: Rates while the ECB file is being
// fetched, then one page of one source.
type paymentsSyncState struct {
	Rates  bool   `json:"rates,omitzero"`
	Source string `json:"source,omitempty"`
	Page   int    `json:"page,omitzero"`
}

type billingPaymentsSteps struct {
	svc BillingPayments
}

// syncPayments does one call per step. A retry starts over: the rates and
// every page are upserts.
func (b billingPaymentsSteps) syncPayments(ctx context.Context, in StepInput) (StepOutput, error) {
	remote := b.svc.Remote()
	sources := b.svc.PaymentSources()
	if remote == nil || len(sources) == 0 {
		return done("skipped_billing_off")
	}
	var st paymentsSyncState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	switch {
	case !st.Rates && st.Source == "":
		return fetchRates()
	case st.Rates:
		b.recordRates(ctx, in.Result)
		return readPayments(ctx, remote, paymentsSyncState{Source: sources[0], Page: 1})
	}
	page, err := providerResult(remote, in.Result, listPayments(ctx, st))
	switch {
	case errors.Is(err, billingdom.ErrNotConfigured):
		return done("skipped_not_configured")
	case err != nil:
		return retry(BillingSyncPaymentsKind, err)
	}
	if err := b.svc.RecordPayments(ctx, page.Payments); err != nil {
		return retry(BillingSyncPaymentsKind, err)
	}
	if st.Page < page.LastPage {
		return readPayments(ctx, remote, paymentsSyncState{Source: st.Source, Page: st.Page + 1})
	}
	if i := slices.Index(sources, st.Source); i >= 0 && i+1 < len(sources) {
		return readPayments(ctx, remote, paymentsSyncState{Source: sources[i+1], Page: 1})
	}
	return done("synced")
}

// fetchRates asks river-service for the ECB's 90-day file.
func fetchRates() (StepOutput, error) {
	raw, err := encodeState(paymentsSyncState{Rates: true})
	if err != nil {
		return StepOutput{}, err
	}
	return StepOutput{State: raw, HTTP: &HTTPCall{Method: http.MethodGet, URL: ecb.HistoryURL}}, nil
}

// recordRates stores the fetched rates. A failure is only logged: payments
// without a rate stay unconverted until a later run.
func (b billingPaymentsSteps) recordRates(ctx context.Context, r *StepResult) {
	var err error
	switch {
	case r == nil:
		err = errors.New("no result")
	case r.Err != "":
		err = errors.New(r.Err)
	case r.Status != http.StatusOK:
		err = errors.New("HTTP " + http.StatusText(r.Status))
	default:
		var rates []billingdom.FXRate
		if rates, err = ecb.Parse(r.Body); err == nil {
			err = b.svc.RecordFXRates(ctx, rates)
		}
	}
	if err != nil {
		slog.WarnContext(ctx, "billing payments sync: exchange rates not refreshed; non-USD payments wait for a later run", logging.AttrComponent, "jobs.billing", logging.AttrError, err)
	}
}

func readPayments(ctx context.Context, remote billingdom.Remote, st paymentsSyncState) (StepOutput, error) {
	return planProviderCall(remote, st, listPayments(ctx, st))
}

func listPayments(ctx context.Context, st paymentsSyncState) func(p billingdom.Provider) (billingdom.PaymentPage, error) {
	return func(p billingdom.Provider) (billingdom.PaymentPage, error) {
		return p.ListPayments(ctx, st.Source, st.Page)
	}
}
