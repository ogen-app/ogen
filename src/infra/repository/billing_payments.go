package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PaymentsQuery selects one page of payments: one mode, occurred in
// [From, To) (either bound optional), after the (AfterAt, AfterID) keyset
// cursor when AfterAt is set.
type PaymentsQuery struct {
	TestMode bool
	From, To *time.Time
	AfterAt  *time.Time
	AfterID  string
	Limit    int
}

// PaymentRow is a payment with its workspace and tier resolved: the tenant
// from the checkout's custom data, else from the mirrored subscription; the
// tier from the variant map, else from the subscription's price. Both are ""
// when unknown.
type PaymentRow struct {
	models.BillingPayment `bun:",extend"`

	ResolvedTenantID string `bun:"resolved_tenant_id"`
	TierID           string `bun:"tier_id"`
}

// paymentUpdateColumns are rewritten when a newer copy of a payment lands.
var paymentUpdateColumns = []string{
	"provider_subscription_id", "provider_variant_id", "billing_reason", "currency",
	"subtotal", "discount", "tax", "total", "refunded",
	"subtotal_usd", "discount_usd", "tax_usd", "total_usd", "refunded_usd",
	"fee", "fee_usd", "fee_is_estimated", "status", "occurred_at", "provider_updated_at",
}

func (r *billingRepository) UpsertPayments(ctx context.Context, rows []models.BillingPayment) error {
	if len(rows) == 0 {
		return nil
	}
	q := r.db.NewInsert().Model(&rows).
		On("CONFLICT (provider, test_mode, source, provider_id, kind) DO UPDATE").
		Set("tenant_id = COALESCE(EXCLUDED.tenant_id, bp.tenant_id)").
		Set("updated_at = now()").
		Where("bp.provider_updated_at <= EXCLUDED.provider_updated_at")
	for _, c := range paymentUpdateColumns {
		q = q.Set("? = EXCLUDED.?", bun.Ident(c), bun.Ident(c))
	}
	_, err := q.Exec(ctx)
	return err
}

// ListPayments leaves out an order that opened a subscription: the
// subscription's initial invoice is the same charge, and counting both would
// double it.
func (r *billingRepository) ListPayments(ctx context.Context, q PaymentsQuery) ([]PaymentRow, error) {
	rows := []PaymentRow{}
	sel := r.db.NewSelect().Model(&rows).
		ColumnExpr("bp.*").
		ColumnExpr("COALESCE(bp.tenant_id, bs.tenant_id, '') AS resolved_tenant_id").
		ColumnExpr("COALESCE(tv.tier_id, '') AS tier_id").
		Join("LEFT JOIN billing_subscriptions AS bs ON bp.provider_subscription_id <> '' AND bs.provider = bp.provider AND bs.provider_subscription_id = bp.provider_subscription_id").
		Join("LEFT JOIN billing_variant_mappings AS bvm ON bvm.test_mode = bp.test_mode AND bvm.variant_id = NULLIF(bp.provider_variant_id, '')").
		Join("LEFT JOIN tenant_tier_version_prices AS tvp ON tvp.id = COALESCE(bvm.tier_version_price_id, bs.tier_version_price_id)").
		Join("LEFT JOIN tenant_tier_versions AS tv ON tv.id = tvp.tier_version_id").
		Where("bp.test_mode = ?", q.TestMode).
		Where(`NOT (bp.source = 'order' AND EXISTS (
			SELECT 1 FROM billing_subscriptions AS so
			WHERE so.provider = bp.provider AND so.provider_order_id = bp.provider_id))`).
		OrderExpr("bp.occurred_at, bp.id").
		Limit(q.Limit)
	if q.From != nil {
		sel = sel.Where("bp.occurred_at >= ?", *q.From)
	}
	if q.To != nil {
		sel = sel.Where("bp.occurred_at < ?", *q.To)
	}
	if q.AfterAt != nil {
		sel = sel.Where("(bp.occurred_at, bp.id) > (?, ?)", *q.AfterAt, q.AfterID)
	}
	if err := sel.Scan(ctx); err != nil {
		return nil, err
	}
	return rows, nil
}
