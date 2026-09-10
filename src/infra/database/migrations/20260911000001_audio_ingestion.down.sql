-- CON-282: narrow the asset-type check back off AUDIO. DOC is kept (it is a
-- live asset type the app writes; the up migration backfilled it), so this only
-- removes AUDIO rather than reverting to the pre-CON-280 set.
ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_type_check;
ALTER TABLE assets
    ADD CONSTRAINT assets_type_check CHECK (type IS NULL OR type IN ('MD', 'PDF', 'URL', 'IMG', 'DOC'));

-- Drop the audio processing-state tables (children first).
DROP TABLE IF EXISTS utterances;
DROP TABLE IF EXISTS audio_segments;
DROP TABLE IF EXISTS audio_extractions;
