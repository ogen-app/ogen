ALTER TABLE assets
    DROP COLUMN IF EXISTS origin_ref,
    DROP COLUMN IF EXISTS origin;
DROP TABLE IF EXISTS plugin_pairings;
DROP TABLE IF EXISTS plugin_tokens;
