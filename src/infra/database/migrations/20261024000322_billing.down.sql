-- The reason check keeps 'subscription_started': assignments written while
-- billing ran stay in place, and closing their range re-checks the constraint.

DROP INDEX IF EXISTS uq_tenant_tiers_free_trial;
ALTER TABLE tenant_tiers DROP COLUMN IF EXISTS free_trial;
ALTER TABLE tenants DROP COLUMN IF EXISTS billing_required;

DROP TABLE IF EXISTS billing_scheduled_changes;
DROP TABLE IF EXISTS billing_webhook_events;
DROP TABLE IF EXISTS billing_subscriptions;
