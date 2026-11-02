-- One pending automatic quality assessment per post. A lifecycle checkpoint
-- upserts the row (the latest trigger and due time win), and the
-- assess_due_posts sweep claims due rows: claimed_until is the claim lease,
-- so a sweep that dies leaves the row to be claimed again once it lapses.
CREATE TABLE IF NOT EXISTS post_quality_queue (
    post_id       TEXT        PRIMARY KEY REFERENCES posts (id) ON DELETE CASCADE,
    tenant_id     TEXT        NOT NULL,
    trigger       TEXT        NOT NULL,
    due_at        TIMESTAMPTZ NOT NULL,
    claimed_until TIMESTAMPTZ,
    attempts      INTEGER     NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS post_quality_queue_due_idx ON post_quality_queue (due_at);
