-- Jobs bound for river-service, written in the same transaction as the change
-- that caused them and delivered by the outbox relay. A row is sent when
-- river-service accepts it; until then the relay retries it with backoff.
CREATE TABLE IF NOT EXISTS job_outbox (
    id               text PRIMARY KEY,
    kind             text NOT NULL,
    -- The jobs.v1 EnqueueRequest (args and scheduled_at) as protojson.
    payload          jsonb NOT NULL,
    -- Trace context of the request that enqueued the job.
    trace_carrier    jsonb,
    request_id       text,
    attempts         integer NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    last_error       text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    sent_at          timestamptz,
    job_id           bigint
);

-- The relay's claim: unsent rows that are due, oldest first.
CREATE INDEX IF NOT EXISTS job_outbox_due_idx
    ON job_outbox (next_attempt_at, created_at)
    WHERE sent_at IS NULL;

-- Retention cleanup of delivered rows.
CREATE INDEX IF NOT EXISTS job_outbox_sent_idx
    ON job_outbox (sent_at)
    WHERE sent_at IS NOT NULL;
