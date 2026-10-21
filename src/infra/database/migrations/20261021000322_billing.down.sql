ALTER TABLE tenant_tier_assignments DROP CONSTRAINT tenant_tier_assignments_reason_check;
ALTER TABLE tenant_tier_assignments ADD CONSTRAINT tenant_tier_assignments_reason_check
    CHECK (reason IN ('signup', 'migration_accepted', 'migration_lapsed', 'grandfathered', 'upgrade', 'downgrade', 'operator_set')) NOT VALID;

DROP INDEX IF EXISTS uq_tenant_tiers_free_trial;
ALTER TABLE tenant_tiers DROP COLUMN IF EXISTS free_trial;
ALTER TABLE tenants DROP COLUMN IF EXISTS billing_required;

DROP TABLE IF EXISTS billing_scheduled_changes;
DROP TABLE IF EXISTS billing_webhook_events;
DROP TABLE IF EXISTS billing_subscriptions;
