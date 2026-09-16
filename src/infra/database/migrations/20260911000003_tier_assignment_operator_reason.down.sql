-- Revert to the CON-243 reason set (drops 'operator_set').
--
-- Assignment history is append-only, so we never rewrite or delete rows to make
-- the rollback fit. Instead, refuse to roll back while any assignment still uses
-- 'operator_set' — those tenants must be reassigned to a legacy reason first.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM tenant_tier_assignments WHERE reason = 'operator_set') THEN
        RAISE EXCEPTION 'cannot roll back 20260911000003: % assignment row(s) use reason ''operator_set''; reassign them first',
            (SELECT count(*) FROM tenant_tier_assignments WHERE reason = 'operator_set');
    END IF;
END $$;

--bun:split

ALTER TABLE tenant_tier_assignments DROP CONSTRAINT tenant_tier_assignments_reason_check;
-- Legacy set. The guard above proved no row uses 'operator_set', so every row
-- satisfies it; NOT VALID avoids the blocking scan.
ALTER TABLE tenant_tier_assignments ADD CONSTRAINT tenant_tier_assignments_reason_check
    CHECK (reason IN ('signup', 'migration_accepted', 'migration_lapsed', 'grandfathered', 'upgrade', 'downgrade')) NOT VALID;
