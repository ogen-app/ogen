-- Full-text search over asset chunks, backing GET /api/content-bank/assets/search
-- ("which assets mention this text"). Lexical on purpose: a filter that says
-- "mentions" must match the words typed, not text that merely means something
-- similar (that is what the embedding column is for).
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

-- Plain (not CONCURRENTLY) build: the migration runs in one transaction.
CREATE INDEX idx_assets_chunks_content_tsv ON assets_chunks USING gin (content_tsv);
