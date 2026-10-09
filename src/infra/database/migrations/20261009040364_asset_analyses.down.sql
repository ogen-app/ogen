-- Drop asset analyses and the ideas link back to them.
DROP INDEX IF EXISTS idx_ideas_asset_analysis;
ALTER TABLE ideas
    DROP COLUMN IF EXISTS asset_analysis_id,
    DROP COLUMN IF EXISTS origin;
DROP TABLE IF EXISTS asset_analysis_runs;
DROP TABLE IF EXISTS asset_analyses;
