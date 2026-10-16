-- A lens's detached run. An analysis river-service orchestrates runs its
-- model call detached inside ogen: the job claims the slot with a token, the
-- run heartbeats while it works and records its outcome, and the job polls
-- the slot, so a deploy of either service never loses track of the call.
ALTER TABLE asset_analyses
    ADD COLUMN IF NOT EXISTS run_token        text,
    ADD COLUMN IF NOT EXISTS run_heartbeat_at timestamptz,
    ADD COLUMN IF NOT EXISTS run_done_at      timestamptz,
    ADD COLUMN IF NOT EXISTS run_error        text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS run_error_code   text NOT NULL DEFAULT '',
    -- Refreshed by every step of the lens's analysis job. The backfill takes
    -- a pending lens without recent activity as stranded and queues it again.
    ADD COLUMN IF NOT EXISTS last_activity_at timestamptz;
