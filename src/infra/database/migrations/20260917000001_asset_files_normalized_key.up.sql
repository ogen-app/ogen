-- CON-299: expose the browser-drawable normalized derivative as a URL on the
-- asset payload. CON-281 widened image ingestion to HEIC/HEIF and TIFF — formats
-- no browser can decode (HEIC: Safari only; TIFF: nowhere). process_image already
-- writes assets/{id}/normalized.png on every run, but its key was recorded only on
-- image_extractions.normalized_s3_key, never minted into a public URL on the asset.
-- Give asset_files its own normalized_s3_key so decorateFile can mint normalized_url
-- the same way it mints url / thumbnail_url, with no extra query on the batch list
-- path (asset_files is already loaded there).
ALTER TABLE asset_files ADD COLUMN normalized_s3_key TEXT;

-- Backfill already-ingested images so they light up without a re-upload. Copy the
-- latest normalized key from a SETTLED-searchable extraction (partial|complete) —
-- never a pending/in-flight or failed run, mirroring the runtime rule that the key
-- is exposed only once a run settles searchable (AC4). One row per asset, newest
-- run wins. The key is already tenant-prefixed, so it round-trips through
-- storage.PublicURL unchanged. Also fill thumbnail_s3_key where a file row has none
-- yet — images carried no thumbnail before this — so the list preview cell draws
-- the derivative instead of the undecodable original. NULLIF treats a blank key as
-- missing so an empty-string thumbnail is backfilled too, not just a NULL one.
UPDATE asset_files af
SET normalized_s3_key = latest.normalized_s3_key,
    thumbnail_s3_key  = COALESCE(NULLIF(af.thumbnail_s3_key, ''), latest.normalized_s3_key)
FROM (
    SELECT DISTINCT ON (asset_id) asset_id, normalized_s3_key
    FROM image_extractions
    WHERE normalized_s3_key IS NOT NULL AND normalized_s3_key <> ''
      AND status IN ('partial', 'complete')
    ORDER BY asset_id, created_at DESC
) AS latest
WHERE af.asset_id = latest.asset_id;
