-- The asset an analysis idea came from, kept on the idea itself: the analysis
-- row it points at is replaced on re-runs and removed with its lens, and a
-- triaged idea outlives both. It goes NULL only when the asset is deleted.
--
-- angle, rationale and sources are the parts the analysis writes into note,
-- as fields. NULL on manual ideas.
ALTER TABLE ideas
    ADD COLUMN IF NOT EXISTS source_asset_id TEXT NULL REFERENCES assets (id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS angle TEXT NULL,
    ADD COLUMN IF NOT EXISTS rationale TEXT NULL,
    ADD COLUMN IF NOT EXISTS sources TEXT[] NULL;

UPDATE ideas i
SET source_asset_id = aa.asset_id
FROM asset_analyses aa
WHERE aa.id = i.asset_analysis_id AND i.source_asset_id IS NULL;

-- Best effort from note, which the analysis writes as the non-empty parts of
-- angle, rationale and "Sources: a, b" joined by blank lines. With both angle
-- and rationale present the split is unambiguous; otherwise they stay NULL.
WITH parts AS (
    SELECT i.id, string_to_array(i.note, E'\n\n') AS p
    FROM ideas i
    WHERE i.origin = 'asset_analysis' AND i.note <> ''
), split AS (
    SELECT p.id,
        p.p[cardinality(p.p)] LIKE 'Sources: %' AS cited,
        p.p
    FROM parts p
), body AS (
    SELECT s.id,
        CASE WHEN s.cited THEN string_to_array(substr(s.p[cardinality(s.p)], 10), ', ') END AS sources,
        CASE WHEN s.cited THEN s.p[1:cardinality(s.p) - 1] ELSE s.p END AS body
    FROM split s
)
UPDATE ideas i
SET sources   = b.sources,
    angle     = CASE WHEN cardinality(b.body) = 2 THEN b.body[1] END,
    rationale = CASE WHEN cardinality(b.body) = 2 THEN b.body[2] END
FROM body b
WHERE b.id = i.id;

CREATE INDEX IF NOT EXISTS idx_ideas_tenant_pile ON ideas (tenant_id, verdict, remind_at);
CREATE INDEX IF NOT EXISTS idx_ideas_tenant_fit ON ideas (tenant_id, fit_score);
CREATE INDEX IF NOT EXISTS idx_ideas_source_asset ON ideas (source_asset_id) WHERE source_asset_id IS NOT NULL;
