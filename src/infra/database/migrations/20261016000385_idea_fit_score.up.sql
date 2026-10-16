-- Fit score of an analysis idea: the model's per-criterion ratings
-- (fit_breakdown) and the 10–100 score the server computed from them with the
-- weights of fit_rubric_version. All NULL for manual ideas and for ideas from
-- analyses that ran before scoring existed.
ALTER TABLE ideas
    ADD COLUMN IF NOT EXISTS fit_score smallint CHECK (fit_score BETWEEN 10 AND 100),
    ADD COLUMN IF NOT EXISTS fit_breakdown jsonb,
    ADD COLUMN IF NOT EXISTS fit_rubric_version smallint;
