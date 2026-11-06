DROP INDEX IF EXISTS ideas_plan_target_date_idx;

-- Ideas of the new origins would break the old check; they become manual.
UPDATE ideas SET origin = '' WHERE origin IN ('content_plan', 'assistant');
ALTER TABLE ideas DROP CONSTRAINT ideas_origin_check;
ALTER TABLE ideas
    ADD CONSTRAINT ideas_origin_check CHECK (origin IN ('', 'asset_analysis'));

ALTER TABLE ideas
    DROP COLUMN plan_target_date,
    DROP COLUMN plan_phase_id,
    DROP COLUMN plan_post_type,
    DROP COLUMN plan_platform_id;
