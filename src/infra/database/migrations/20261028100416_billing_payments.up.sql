-- Lemon Squeezy money movements, for Harbor's revenue view. One row per
-- order or subscription invoice, and one refund row per order or invoice that
-- has been refunded, holding the amount refunded so far. Amounts are in minor
-- units of the payment's currency, each with its *_usd counterpart in US
-- cents as the provider converted it. Webhooks and the payments sync both
-- upsert here, so a replay or an overlap never adds a row.
CREATE TABLE billing_payments (
    id                        TEXT        PRIMARY KEY,
    provider                  TEXT        NOT NULL,
    -- order | subscription_invoice: the provider object the row comes from.
    -- Order and invoice ids are separate id spaces.
    source                    TEXT        NOT NULL CHECK (source IN ('order', 'subscription_invoice')),
    -- The order or invoice id; a refund row carries the id it reverses.
    provider_id               TEXT        NOT NULL,
    kind                      TEXT        NOT NULL CHECK (kind IN ('order', 'subscription_invoice', 'refund', 'chargeback')),
    test_mode                 BOOLEAN     NOT NULL,
    -- From the checkout's custom data; NULL when it carried none. Reads fall
    -- back to the mirrored subscription.
    tenant_id                 TEXT,
    provider_subscription_id  TEXT        NOT NULL DEFAULT '',
    provider_variant_id       TEXT        NOT NULL DEFAULT '',
    -- initial | renewal | updated for an invoice; '' for an order.
    billing_reason            TEXT        NOT NULL DEFAULT '',
    currency                  TEXT        NOT NULL,
    subtotal                  BIGINT      NOT NULL,
    discount                  BIGINT      NOT NULL,
    tax                       BIGINT      NOT NULL,
    total                     BIGINT      NOT NULL,
    refunded                  BIGINT      NOT NULL,
    subtotal_usd              BIGINT      NOT NULL,
    discount_usd              BIGINT      NOT NULL,
    tax_usd                   BIGINT      NOT NULL,
    total_usd                 BIGINT      NOT NULL,
    refunded_usd              BIGINT      NOT NULL,
    -- The provider does not report its fee: it is estimated from the
    -- configured formula until payout reconciliation replaces it.
    fee                       BIGINT      NOT NULL,
    fee_usd                   BIGINT      NOT NULL,
    fee_is_estimated          BOOLEAN     NOT NULL,
    status                    TEXT        NOT NULL,
    occurred_at               TIMESTAMPTZ NOT NULL,
    -- The provider's own last change: an older copy never overwrites a newer.
    provider_updated_at       TIMESTAMPTZ NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_billing_payments_provider UNIQUE (provider, test_mode, source, provider_id, kind)
);

-- ListPayments: one mode, a time range, keyset by (occurred_at, id).
CREATE INDEX billing_payments_list_idx ON billing_payments (test_mode, occurred_at, id);
