-- A distill run's detached model call. When river-service orchestrates an
-- idea or brand distill, the model call runs detached inside ogen: the job
-- claims the run's slot with a token, the call heartbeats while it works and
-- records its outcome, and the job polls the slot, so a deploy of either
-- service never loses track of the call.
ALTER TABLE idea_distill_runs
    ADD COLUMN IF NOT EXISTS run_token        text,
    ADD COLUMN IF NOT EXISTS run_heartbeat_at timestamptz,
    ADD COLUMN IF NOT EXISTS run_done_at      timestamptz,
    ADD COLUMN IF NOT EXISTS run_error        text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS run_error_code   text NOT NULL DEFAULT '';

ALTER TABLE brand_distill_runs
    ADD COLUMN IF NOT EXISTS run_token        text,
    ADD COLUMN IF NOT EXISTS run_heartbeat_at timestamptz,
    ADD COLUMN IF NOT EXISTS run_done_at      timestamptz,
    ADD COLUMN IF NOT EXISTS run_error        text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS run_error_code   text NOT NULL DEFAULT '';
