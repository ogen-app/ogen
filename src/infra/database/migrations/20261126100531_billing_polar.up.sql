-- Polar replaces Lemon Squeezy as merchant of record.
--
-- Expand only: this adds what the Polar build needs and changes nothing the
-- previous build reads, so rolling back to it stays possible. The Polar build
-- writes provider_product_id and keeps provider_variant_id equal to it; the
-- Lemon Squeezy columns, the variant mappings and the body-hash dedupe are
-- dropped by a later migration, once no build reads them.

-- Subscriptions: the provider product, the provider's raw status, and the
-- change Polar holds for the next period.
ALTER TABLE billing_subscriptions DROP CONSTRAINT billing_subscriptions_provider_check;
ALTER TABLE billing_subscriptions ADD CONSTRAINT billing_subscriptions_provider_check
    CHECK (provider IN ('lemonsqueezy', 'polar'));
ALTER TABLE billing_subscriptions
    ADD COLUMN provider_product_id  TEXT        NOT NULL DEFAULT '',
    ADD COLUMN provider_status      TEXT        NOT NULL DEFAULT '',
    ADD COLUMN cancel_at_period_end BOOLEAN     NOT NULL DEFAULT false,
    ADD COLUMN pending_product_id   TEXT        NOT NULL DEFAULT '',
    ADD COLUMN pending_effective_at TIMESTAMPTZ;
UPDATE billing_subscriptions SET provider_product_id = provider_variant_id WHERE provider_product_id = '';

-- Webhooks: Polar sends an event id (webhook-id), the same on every
-- redelivery, so it is the dedupe key. body_sha256 stays unique for the
-- previous build, which inserts on it.
ALTER TABLE billing_webhook_events ADD COLUMN provider_event_id TEXT;
CREATE UNIQUE INDEX uq_billing_webhook_events_provider_event ON billing_webhook_events (provider, provider_event_id)
    WHERE provider_event_id IS NOT NULL;

-- Which Polar product sells each tier version price row, per mode. A product
-- bills one interval and may carry a fixed price in several currencies, so a
-- product sells at most one price per currency and mode: webhooks resolve the
-- price from the subscription's product and currency. Lemon Squeezy variant
-- mappings are not copied: they name Lemon Squeezy objects.
CREATE TABLE billing_product_mappings (
    tier_version_price_id TEXT        NOT NULL REFERENCES tenant_tier_version_prices (id) ON DELETE CASCADE,
    test_mode             BOOLEAN     NOT NULL,
    product_id            TEXT        NOT NULL CHECK (length(product_id) > 0),
    -- The price row's currency, kept here so the uniqueness below holds.
    currency              TEXT        NOT NULL CHECK (length(currency) > 0),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by            TEXT        NOT NULL,
    reason                TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (tier_version_price_id, test_mode),
    CONSTRAINT uq_billing_product_mappings_product UNIQUE (product_id, test_mode, currency)
);

-- Payments: Polar refunds are their own objects (one row each), Polar reports
-- no USD amounts, so a non-USD payment is converted at the ECB rate of its
-- day and its *_usd columns stay NULL until that rate is known. fx_* record
-- the conversion; Lemon Squeezy rows keep NULL there (it converted itself).
ALTER TABLE billing_payments DROP CONSTRAINT billing_payments_source_check;
ALTER TABLE billing_payments ADD CONSTRAINT billing_payments_source_check
    CHECK (source IN ('order', 'subscription_invoice', 'refund'));
ALTER TABLE billing_payments
    ADD COLUMN provider_product_id TEXT          NOT NULL DEFAULT '',
    ADD COLUMN fx_rate             NUMERIC(20, 10),
    ADD COLUMN fx_source           TEXT          CHECK (fx_source IN ('identity', 'ecb', 'polar')),
    ADD COLUMN fx_date             DATE,
    ALTER COLUMN subtotal_usd DROP NOT NULL,
    ALTER COLUMN discount_usd DROP NOT NULL,
    ALTER COLUMN tax_usd      DROP NOT NULL,
    ALTER COLUMN total_usd    DROP NOT NULL,
    ALTER COLUMN refunded_usd DROP NOT NULL,
    ALTER COLUMN fee_usd      DROP NOT NULL;
UPDATE billing_payments SET provider_product_id = provider_variant_id WHERE provider_product_id = '';

-- Billing emails that name Lemon Squeezy or its test period. Boot seeds a
-- template only when its row is absent, so the stale rows are dropped and the
-- next boot seeds the provider-neutral copy.
DELETE FROM email_templates WHERE key = 'billing_payment_failed' AND text LIKE '%Lemon Squeezy%';
DELETE FROM email_templates WHERE key = 'billing_reentry_required' AND text LIKE '%test period%';

-- European Central Bank euro reference rates: per_eur units of currency buy
-- one euro on rate_date (business days only; EUR itself is 1). Global, not
-- tenant scoped. The payments sync refreshes it from the ECB's files.
CREATE TABLE fx_rates (
    rate_date  DATE            NOT NULL,
    currency   TEXT            NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    per_eur    NUMERIC(20, 10) NOT NULL CHECK (per_eur > 0),
    fetched_at TIMESTAMPTZ     NOT NULL DEFAULT now(),
    PRIMARY KEY (rate_date, currency)
);
