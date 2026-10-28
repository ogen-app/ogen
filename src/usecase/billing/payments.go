package billing

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Payments are Lemon Squeezy's money movements, kept for Harbor's revenue
// view. Webhooks record each order and invoice as it happens; the payments
// sync re-reads the whole history, which backfills what predates this table
// and picks up refunds whose webhook was lost. Both paths upsert, so neither
// adds a row the other wrote.

// ErrBadPageToken: a ListPayments page token that is not one this service
// issued (400).
var ErrBadPageToken = errors.New("invalid page token")

const (
	defaultPaymentsPage = 100
	maxPaymentsPage     = 500
)

// RecordPayments upserts payments, with the fee estimated by the configured
// formula.
func (s *Service) RecordPayments(ctx context.Context, payments []billingdom.Payment) error {
	if len(payments) == 0 {
		return nil
	}
	rows := make([]models.BillingPayment, 0, len(payments))
	seen := make(map[string]int, len(payments))
	for _, p := range payments {
		row, err := s.paymentRow(p)
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

func (s *Service) paymentRow(p billingdom.Payment) (models.BillingPayment, error) {
	id, err := models.NewID()
	if err != nil {
		return models.BillingPayment{}, err
	}
	fee, feeUSD := s.d.Fees.Estimate(p)
	row := models.BillingPayment{
		ID: id, Provider: s.provider(), Source: p.Source, ProviderID: p.ProviderID, Kind: p.Kind, TestMode: p.TestMode,
		ProviderSubscriptionID: p.SubscriptionID, ProviderVariantID: p.VariantID, BillingReason: p.BillingReason,
		Currency: strings.ToUpper(p.Currency),
		Subtotal: p.Subtotal, Discount: p.Discount, Tax: p.Tax, Total: p.Total, Refunded: p.Refunded,
		SubtotalUSD: p.SubtotalUSD, DiscountUSD: p.DiscountUSD, TaxUSD: p.TaxUSD, TotalUSD: p.TotalUSD, RefundedUSD: p.RefundedUSD,
		Fee: fee, FeeUSD: feeUSD, FeeIsEstimated: true,
		Status: p.Status, OccurredAt: p.OccurredAt.UTC(), ProviderUpdatedAt: p.UpdatedAt.UTC(),
	}
	if p.TenantID != "" {
		row.TenantID = new(p.TenantID)
	}
	return row, nil
}

// recordWebhookPayments keeps the payments a webhook reports, attributed to
// the checkout's workspace when its custom data names one.
func (s *Service) recordWebhookPayments(ctx context.Context, ev billingdom.Event) error {
	if len(ev.Payments) == 0 {
		return nil
	}
	tenantID := ev.CustomData["tenant_id"]
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

// PaymentsStore returns the store whose payments the sync reads, and false
// while billing is off.
func (s *Service) PaymentsStore() (string, bool) {
	if !s.Enabled() {
		return "", false
	}
	return s.d.Settings.StoreID, true
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
