DROP INDEX IF EXISTS idx_ideas_source_asset;
DROP INDEX IF EXISTS idx_ideas_tenant_fit;
DROP INDEX IF EXISTS idx_ideas_tenant_pile;

ALTER TABLE ideas
    DROP COLUMN IF EXISTS sources,
    DROP COLUMN IF EXISTS rationale,
    DROP COLUMN IF EXISTS angle,
    DROP COLUMN IF EXISTS source_asset_id;
