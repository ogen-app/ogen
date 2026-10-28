package queues

import (
	"context"
	"errors"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/infra/billing/lemonsqueezy"
)

// BillingSyncPaymentsKind pages through the store's whole payment history,
// orders first and then subscription invoices, and upserts every page. Run
// once it backfills the payments that predate the webhooks; run on a schedule
// it picks up refunds whose webhook was lost. river-service schedules it and
// makes each call with the leased key of the current billing mode.
const BillingSyncPaymentsKind = "billing_sync_payments"

// BillingPayments is the billing usecase surface the payments sync drives.
// *billing.Service implements it.
type BillingPayments interface {
	// PaymentsStore returns the store to read, and false while billing is
	// off.
	PaymentsStore() (string, bool)
	RecordPayments(ctx context.Context, payments []billingdom.Payment) error
}

// paymentsSyncSources are read in order.
var paymentsSyncSources = []string{billingdom.PaymentSourceOrder, billingdom.PaymentSourceSubscriptionInvoice}

type paymentsSyncState struct {
	Source string `json:"source,omitempty"`
	Page   int    `json:"page,omitzero"`
}

type billingPaymentsSteps struct {
	svc BillingPayments
}

// syncPayments asks for one page, records it and moves on, one call per
// step. A retry starts over from the first page: every page is an upsert.
func (b billingPaymentsSteps) syncPayments(ctx context.Context, in StepInput) (StepOutput, error) {
	storeID, ok := b.svc.PaymentsStore()
	if !ok {
		return done("skipped_billing_off")
	}
	var st paymentsSyncState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	if st.Source == "" {
		return b.read(ctx, storeID, paymentsSyncState{Source: paymentsSyncSources[0], Page: 1})
	}
	page, err := lemonSqueezyResult(in.Result, b.list(ctx, storeID, st))
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
		return b.read(ctx, storeID, paymentsSyncState{Source: st.Source, Page: st.Page + 1})
	}
	for i, source := range paymentsSyncSources[:len(paymentsSyncSources)-1] {
		if source == st.Source {
			return b.read(ctx, storeID, paymentsSyncState{Source: paymentsSyncSources[i+1], Page: 1})
		}
	}
	return done("synced")
}

func (b billingPaymentsSteps) list(ctx context.Context, storeID string, st paymentsSyncState) func(c *lemonsqueezy.Client) (billingdom.PaymentPage, error) {
	return func(c *lemonsqueezy.Client) (billingdom.PaymentPage, error) {
		return c.ListPayments(ctx, storeID, st.Source, st.Page)
	}
}

func (b billingPaymentsSteps) read(ctx context.Context, storeID string, st paymentsSyncState) (StepOutput, error) {
	op := b.list(ctx, storeID, st)
	call, err := lemonsqueezy.Plan(func(c *lemonsqueezy.Client) error { _, err := op(c); return err })
	if err != nil {
		return StepOutput{}, err
	}
	return lemonSqueezyStep(st, call)
}
