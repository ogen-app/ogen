-- CON-294: Harbor's SetTenantTier now stamps a tenant_tier_assignments row (so
-- tenants.tier_id and the tenant's open assignment never drift), and that
-- assignment carries reason 'operator_set'. Extend the reason CHECK added by
-- 20260911000002 to allow it.
ALTER TABLE tenant_tier_assignments DROP CONSTRAINT tenant_tier_assignments_reason_check;
ALTER TABLE tenant_tier_assignments ADD CONSTRAINT tenant_tier_assignments_reason_check
    CHECK (reason IN ('signup', 'migration_accepted', 'migration_lapsed', 'grandfathered', 'upgrade', 'downgrade', 'operator_set'));
