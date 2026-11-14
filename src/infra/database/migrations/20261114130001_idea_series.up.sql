-- The series a content plan reserved an idea for. A post made from the idea
-- takes it as its series_id. Series are soft-deleted, so this survives a
-- series delete; SET NULL only covers a hard delete (tenant teardown).
ALTER TABLE ideas
    ADD COLUMN series_id TEXT NULL REFERENCES brand_series (id) ON DELETE SET NULL;
CREATE INDEX idx_ideas_series ON ideas (series_id) WHERE series_id IS NOT NULL;
