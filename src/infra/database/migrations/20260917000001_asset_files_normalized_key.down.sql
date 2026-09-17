-- CON-299 down: drop the normalized_s3_key column. The backfill's thumbnail_s3_key
-- fill is intentionally NOT reverted — a thumbnail key is harmless (decorateFile
-- just mints thumbnail_url from it) and recovering which rows it touched is not
-- worth it.
ALTER TABLE asset_files DROP COLUMN IF EXISTS normalized_s3_key;
