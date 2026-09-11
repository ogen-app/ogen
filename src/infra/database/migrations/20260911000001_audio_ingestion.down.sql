-- CON-282: narrow the asset-type check back off AUDIO. DOC is kept (it is a
-- live asset type the app writes; the up migration backfilled it), so this only
-- removes AUDIO rather than reverting to the pre-CON-280 set. NOT VALID: this is
-- a NARROWING, so validating it would fail if any AUDIO asset still exists —
-- purging/converting AUDIO rows before a later VALIDATE is left to the operator.
ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_type_check;
ALTER TABLE assets
    ADD CONSTRAINT assets_type_check CHECK (type IS NULL OR type IN ('MD', 'PDF', 'URL', 'IMG', 'DOC')) NOT VALID;

-- Drop the audio processing-state tables (children first).
DROP TABLE IF EXISTS utterances;
DROP TABLE IF EXISTS audio_segments;
DROP TABLE IF EXISTS audio_extractions;
