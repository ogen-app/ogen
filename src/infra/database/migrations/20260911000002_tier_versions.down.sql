-- CON-243 rollback. Drop triggers + functions first, then the tables in reverse
-- FK order, then the seeded commercial tiers. btree_gist is left installed
-- (harmless, and other features may adopt it).
DROP TRIGGER IF EXISTS trg_tenant_tier_version_prices_immutable ON tenant_tier_version_prices;
DROP TRIGGER IF EXISTS trg_tenant_tier_versions_immutable ON tenant_tier_versions;

--bun:split

DROP FUNCTION IF EXISTS tenant_tier_version_prices_immutable();
DROP FUNCTION IF EXISTS tenant_tier_versions_immutable();

--bun:split

DROP TABLE IF EXISTS tenant_tier_assignments;
DROP TABLE IF EXISTS tenant_tier_version_prices;
DROP TABLE IF EXISTS tenant_tier_versions;

--bun:split

-- Reversible only while no tenant has been reassigned onto these tiers (the
-- tenants.tier_id FK is ON DELETE RESTRICT, so a lingering reference fails the
-- rollback loudly, which is correct).
DELETE FROM tenant_tiers WHERE id IN ('trial', 'pro', 'max');
