DROP INDEX IF EXISTS idx_ideas_asset_analysis;
CREATE INDEX idx_ideas_asset_analysis
    ON ideas (asset_analysis_id) WHERE asset_analysis_id IS NOT NULL;
DROP INDEX IF EXISTS idx_asset_analyses_tenant_asset;

DROP INDEX IF EXISTS idx_assets_tenant_status_type;
DROP INDEX IF EXISTS idx_assets_tenant_updated;

DROP INDEX IF EXISTS idx_assets_chunks_content_tsv;
