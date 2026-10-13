-- Drop full-text search over asset chunks.
DROP INDEX IF EXISTS idx_assets_chunks_content_tsv;
ALTER TABLE assets_chunks
    DROP COLUMN IF EXISTS content_tsv;
