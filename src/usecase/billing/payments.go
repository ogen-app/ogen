package billing

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Payments are the provider's money movements, kept for Harbor's revenue
// view. Webhooks record each order and refund as it happens; the payments
// sync re-reads the whole history, which backfills what predates this table,
// picks up refunds whose webhook was lost and converts payments whose
// exchange rate was not known yet. Both paths upsert, so neither adds a row
// the other wrote.
//
// Polar reports amounts in the payment's currency only. A non-USD payment is
// converted at the ECB reference rate of its day (billingdom.FXTable); until
// that rate is stored its *_usd columns stay NULL.

// ErrBadPageToken: a ListPayments page token that is not one this service
// issued (400).
var ErrBadPageToken = errors.New("invalid page token")

const (
	defaultPaymentsPage = 100
	maxPaymentsPage     = 500
	// fxLookback is how far before the oldest payment rates are read, so a
	// payment after a long holiday still finds the business day before it.
	fxLookback = 14 * 24 * time.Hour
)

// RecordPayments upserts payments, converted to USD where the rate is known
// and with the fee the provider reported or the configured formula's
// estimate.
func (s *Service) RecordPayments(ctx context.Context, payments []billingdom.Payment) error {
	if len(payments) == 0 {
		return nil
	}
	table, err := s.fxTable(ctx, payments)
	if err != nil {
		return err
	}
	rows := make([]models.BillingPayment, 0, len(payments))
	seen := make(map[string]int, len(payments))
	for _, p := range payments {
		row, err := s.paymentRow(p, table)
		if err != nil {
			return err
		}
		// One statement cannot upsert a key twice: keep the newest copy.
		key := strings.Join([]string{row.Source, row.ProviderID, row.Kind, strconv.FormatBool(row.TestMode)}, "\x00")
		if i, ok := seen[key]; ok {
			if row.ProviderUpdatedAt.After(rows[i].ProviderUpdatedAt) {
				rows[i] = row
			}
			continue
		}
		seen[key] = len(rows)
		rows = append(rows, row)
	}
	return s.d.Repo.UpsertPayments(ctx, rows)
}

// fxTable reads the rates the payments' conversions need.
func (s *Service) fxTable(ctx context.Context, payments []billingdom.Payment) (billingdom.FXTable, error) {
	var (
		currencies []string
		earliest   time.Time
	)
	for _, p := range payments {
		c := strings.ToUpper(p.Currency)
		if c == "USD" {
			continue
		}
		if !slices.Contains(currencies, c) {
			currencies = append(currencies, c)
		}
		if earliest.IsZero() || p.OccurredAt.Before(earliest) {
			earliest = p.OccurredAt
		}
	}
	if len(currencies) == 0 {
		return billingdom.NewFXTable(nil), nil
	}
	rates, err := s.d.Repo.FXRates(ctx, currencies, earliest.Add(-fxLookback))
	if err != nil {
		return billingdom.FXTable{}, err
	}
	return billingdom.NewFXTable(rates), nil
}

func (s *Service) paymentRow(p billingdom.Payment, table billingdom.FXTable) (models.BillingPayment, error) {
	id, err := models.NewID()
	if err != nil {
		return models.BillingPayment{}, err
	}
	row := models.BillingPayment{
		ID: id, Provider: s.provider(), Source: p.Source, ProviderID: p.ProviderID, Kind: p.Kind, TestMode: p.TestMode,
		// provider_variant_id is what the previous release reads.
		ProviderSubscriptionID: p.SubscriptionID, ProviderProductID: p.ProductID, ProviderVariantID: p.ProductID, BillingReason: p.BillingReason,
		Currency: strings.ToUpper(p.Currency),
		Subtotal: p.Subtotal, Discount: p.Discount, Tax: p.Tax, Total: p.Total, Refunded: p.Refunded,
		FeeIsEstimated: p.Fee == nil,
		Status:         p.Status, OccurredAt: p.OccurredAt.UTC(), ProviderUpdatedAt: p.UpdatedAt.UTC(),
	}
	if p.Fee != nil {
		row.Fee = *p.Fee
	}
	if p.TenantID != "" {
		row.TenantID = new(p.TenantID)
	}
	conv, err := table.ToUSD(row.Currency, row.OccurredAt)
	if errors.Is(err, billingdom.ErrNoRate) {
		return row, nil // converted by a later sync, once the rate is stored
	}
	if err != nil {
		return models.BillingPayment{}, err
	}
	row.SubtotalUSD, row.DiscountUSD, row.TaxUSD = new(conv.Cents(p.Subtotal)), new(conv.Cents(p.Discount)), new(conv.Cents(p.Tax))
	row.TotalUSD, row.RefundedUSD = new(conv.Cents(p.Total)), new(conv.Cents(p.Refunded))
	row.FXRate, row.FXSource = new(conv.RateString()), new(conv.Source)
	if !conv.Date.IsZero() {
		row.FXDate = new(conv.Date)
	}
	if p.Fee != nil {
		row.FeeUSD = new(conv.Cents(*p.Fee))
	} else {
		fee, feeUSD := s.d.Fees.Estimate(p.Kind, p.Total, *row.TotalUSD)
		row.Fee, row.FeeUSD = fee, new(feeUSD)
	}
	return row, nil
}

// RecordFXRates stores reference rates for the conversions.
func (s *Service) RecordFXRates(ctx context.Context, rates []billingdom.FXRate) error {
	return s.d.Repo.UpsertFXRates(ctx, rates)
}

// recordWebhookPayments keeps the payments a webhook reports, attributed to
// the workspace the customer or the checkout's metadata names.
func (s *Service) recordWebhookPayments(ctx context.Context, ev billingdom.Event) error {
	if len(ev.Payments) == 0 {
		return nil
	}
	tenantID := cmp.Or(ev.CustomerExternalID, ev.Metadata["tenant_id"])
	payments := make([]billingdom.Payment, len(ev.Payments))
	for i, p := range ev.Payments {
		p.TenantID = cmp.Or(p.TenantID, tenantID)
		payments[i] = p
	}
	if err := s.RecordPayments(ctx, payments); err != nil {
		return fmt.Errorf("billing webhook: record payments: %w", err)
	}
	return nil
}

// PaymentSources lists the provider's payment histories the sync reads, in
// order; none while billing is off.
func (s *Service) PaymentSources() []string {
	if !s.Enabled() {
		return nil
	}
	return s.d.Provider.PaymentSources()
}

// PaymentRow is one listed payment.
type PaymentRow = repository.PaymentRow

// ListPayments pages one mode's payments that occurred in [from, to), oldest
// first. A nil bound is open. The token is the opaque cursor of the previous
// page; the next one is "" after the last page.
func (s *Service) ListPayments(ctx context.Context, testMode bool, from, to *time.Time, pageSize int, pageToken string) ([]PaymentRow, string, error) {
	if pageSize <= 0 {
		pageSize = defaultPaymentsPage
	}
	pageSize = min(pageSize, maxPaymentsPage)
	q := repository.PaymentsQuery{TestMode: testMode, From: from, To: to, Limit: pageSize + 1}
	if pageToken != "" {
		at, id, err := decodePaymentsToken(pageToken)
		if err != nil {
			return nil, "", err
		}
		q.AfterAt, q.AfterID = &at, id
	}
	rows, err := s.d.Repo.ListPayments(ctx, q)
	if err != nil {
		return nil, "", err
	}
	if len(rows) <= pageSize {
		return rows, "", nil
	}
	rows = rows[:pageSize]
	last := rows[len(rows)-1]
	return rows, encodePaymentsToken(last.OccurredAt, last.ID), nil
}

// The page token is the last row's (occurred_at, id), so a page never skips
// or repeats a row that another write lands next to.
func encodePaymentsToken(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%d|%s", at.UnixNano(), id))
}

func decodePaymentsToken(token string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", ErrBadPageToken
	}
	nanos, id, ok := strings.Cut(string(raw), "|")
	n, err := strconv.ParseInt(nanos, 10, 64)
	if !ok || err != nil || id == "" {
		return time.Time{}, "", ErrBadPageToken
	}
	return time.Unix(0, n).UTC(), id, nil
}
