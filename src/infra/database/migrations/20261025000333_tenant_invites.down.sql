DELETE FROM tenant_tier_assignments WHERE reason = 'comp';
ALTER TABLE tenant_tier_assignments DROP CONSTRAINT tenant_tier_assignments_reason_check;
ALTER TABLE tenant_tier_assignments ADD CONSTRAINT tenant_tier_assignments_reason_check
    CHECK (reason IN ('signup', 'migration_accepted', 'migration_lapsed', 'grandfathered', 'upgrade', 'downgrade', 'operator_set', 'subscription_started')) NOT VALID;

ALTER TABLE tenants
    DROP CONSTRAINT IF EXISTS tenants_complimentary_not_billed,
    DROP COLUMN IF EXISTS complimentary,
    DROP COLUMN IF EXISTS invite_id;

DROP TABLE IF EXISTS tenant_invite_groups;
DROP TABLE IF EXISTS tenant_invites;
DROP TABLE IF EXISTS platform_signup_settings;
