-- content_hash fingerprints the scored post text (title, body, platform,
-- type, media flag); context_hash fingerprints everything else the model saw
-- (rubric, campaign brief, phase, assets, model, weights). Reads compare both
-- with the post's current state to report a stale assessment, and automatic
-- runs re-score only when the content changed.
--
-- model_runs counts the model runs behind the row (cached reuses excluded);
-- the quality_reviews_per_post entitlement caps it. Existing rows each stand
-- for at least one run.
--
-- trigger records what started the run that produced the row: manual, or one
-- of the automatic lifecycle checkpoints.
ALTER TABLE post_evaluations
    ADD COLUMN IF NOT EXISTS content_hash TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS context_hash TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS model_runs   INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS trigger      TEXT    NOT NULL DEFAULT 'manual';

UPDATE post_evaluations SET model_runs = 1 WHERE model_runs = 0;
