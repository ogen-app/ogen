-- Restores the global name uniqueness. The per-workspace clones made by the up
-- backfill are not merged back, so this fails while two workspaces hold a type
-- with the same name — rename or remove one first.
DROP INDEX IF EXISTS campaigns_types_system_name_key;
DROP INDEX IF EXISTS campaigns_types_tenant_name_key;
ALTER TABLE campaigns_types DROP CONSTRAINT IF EXISTS campaigns_types_owner_matches_system;
ALTER TABLE campaigns_types ADD CONSTRAINT campaigns_types_name_key UNIQUE (name);
ALTER TABLE campaigns_types DROP COLUMN IF EXISTS tenant_id;
