-- A recording's length on the file, so the Assets list can show "18 min"
-- without reading the audio pipeline. Same name and type as
-- post_attachments.duration_ms. 0 means unknown or not a recording.
ALTER TABLE asset_files ADD COLUMN duration_ms BIGINT NOT NULL DEFAULT 0;

-- Backfill from each asset's latest extraction run that measured the source.
UPDATE asset_files af
SET duration_ms = latest.source_duration_ms
FROM (
    SELECT DISTINCT ON (asset_id) asset_id, source_duration_ms
    FROM audio_extractions
    WHERE source_duration_ms > 0
    ORDER BY asset_id, created_at DESC
) latest
WHERE af.asset_id = latest.asset_id;

-- One sentence saying what the source offers, written by the analysis. Rows
-- analysed before this column existed keep '' and the read path derives a
-- sentence from the summary instead.
ALTER TABLE asset_analyses ADD COLUMN gist TEXT NOT NULL DEFAULT '';
