-- Revert to the CON-243 reason set (drops 'operator_set'). Rolls back cleanly
-- only when no assignment currently uses 'operator_set'.
ALTER TABLE tenant_tier_assignments DROP CONSTRAINT tenant_tier_assignments_reason_check;
ALTER TABLE tenant_tier_assignments ADD CONSTRAINT tenant_tier_assignments_reason_check
    CHECK (reason IN ('signup', 'migration_accepted', 'migration_lapsed', 'grandfathered', 'upgrade', 'downgrade'));
