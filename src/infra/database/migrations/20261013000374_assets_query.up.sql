-- Server-side Assets query: GET /api/content-bank/assets filters, counts and
-- pages the workspace's assets, and searches the text inside them.

-- Full-text search over asset chunks ("which assets mention this text").
-- Lexical on purpose: a filter that says "mentions" must match the words
-- typed, not text that merely means something similar (that is what the
-- embedding column is for).
--
-- content_tsv is a STORED generated column, so the ALTER fills it for every
-- existing chunk and UpsertChunks' inserts get it on write with no code change.
-- The 'english' config stems ("cycles" finds "cycle") and drops stopwords;
-- non-English words pass through unstemmed, so they still match exactly.
-- Adding a stored generated column rewrites the table under an exclusive lock,
-- which is fine at today's chunk counts.
ALTER TABLE assets_chunks
    ADD COLUMN content_tsv tsvector
    GENERATED ALWAYS AS (to_tsvector('english', content)) STORED;

-- Plain (not CONCURRENTLY) builds: the migration runs in one transaction.
CREATE INDEX idx_assets_chunks_content_tsv ON assets_chunks USING gin (content_tsv);

-- The default order of the list, and its keyset cursor.
CREATE INDEX idx_assets_tenant_updated ON assets (tenant_id, updated_at DESC, id DESC);
CREATE INDEX idx_assets_tenant_status_type ON assets (tenant_id, status, type);

-- Idea counts per asset: every analysis lens of the tenant, then the ideas of
-- each lens. The covering columns let both be read from the index alone.
CREATE INDEX idx_asset_analyses_tenant_asset
    ON asset_analyses (tenant_id, asset_id) INCLUDE (campaign_id, status);
DROP INDEX IF EXISTS idx_ideas_asset_analysis;
CREATE INDEX idx_ideas_asset_analysis
    ON ideas (asset_analysis_id) INCLUDE (origin, verdict, remind_at)
    WHERE asset_analysis_id IS NOT NULL;
