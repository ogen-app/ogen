ALTER TABLE ideas
    DROP COLUMN IF EXISTS fit_score,
    DROP COLUMN IF EXISTS fit_breakdown,
    DROP COLUMN IF EXISTS fit_rubric_version;
