DROP TABLE IF EXISTS fx_rates;

UPDATE billing_payments SET subtotal_usd = 0 WHERE subtotal_usd IS NULL;
UPDATE billing_payments SET discount_usd = 0 WHERE discount_usd IS NULL;
UPDATE billing_payments SET tax_usd = 0 WHERE tax_usd IS NULL;
UPDATE billing_payments SET total_usd = 0 WHERE total_usd IS NULL;
UPDATE billing_payments SET refunded_usd = 0 WHERE refunded_usd IS NULL;
UPDATE billing_payments SET fee_usd = 0 WHERE fee_usd IS NULL;
ALTER TABLE billing_payments
    DROP COLUMN IF EXISTS fx_date,
    DROP COLUMN IF EXISTS fx_source,
    DROP COLUMN IF EXISTS fx_rate,
    DROP COLUMN IF EXISTS provider_product_id,
    ALTER COLUMN subtotal_usd SET NOT NULL,
    ALTER COLUMN discount_usd SET NOT NULL,
    ALTER COLUMN tax_usd      SET NOT NULL,
    ALTER COLUMN total_usd    SET NOT NULL,
    ALTER COLUMN refunded_usd SET NOT NULL,
    ALTER COLUMN fee_usd      SET NOT NULL;
-- Polar refund rows may exist: the narrower check only binds new rows.
ALTER TABLE billing_payments DROP CONSTRAINT billing_payments_source_check;
ALTER TABLE billing_payments ADD CONSTRAINT billing_payments_source_check
    CHECK (source IN ('order', 'subscription_invoice')) NOT VALID;

DROP TABLE IF EXISTS billing_product_mappings;

DROP INDEX IF EXISTS uq_billing_webhook_events_provider_event;
ALTER TABLE billing_webhook_events DROP COLUMN IF EXISTS provider_event_id;

ALTER TABLE billing_subscriptions
    DROP COLUMN IF EXISTS pending_effective_at,
    DROP COLUMN IF EXISTS pending_product_id,
    DROP COLUMN IF EXISTS cancel_at_period_end,
    DROP COLUMN IF EXISTS provider_status,
    DROP COLUMN IF EXISTS provider_product_id;
-- Polar subscriptions may exist: the narrower check only binds new rows.
ALTER TABLE billing_subscriptions DROP CONSTRAINT billing_subscriptions_provider_check;
ALTER TABLE billing_subscriptions ADD CONSTRAINT billing_subscriptions_provider_check
    CHECK (provider IN ('lemonsqueezy')) NOT VALID;
