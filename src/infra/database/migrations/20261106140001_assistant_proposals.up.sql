-- Work the assistant offered but has not done yet. A proposal-tier tool call
-- is stored here instead of running; confirming it (a button or a "yes" in
-- the chat) runs the stored tool with the stored arguments, so the model never
-- re-plans an offer the user accepted. scope_id is polymorphic (a campaign or
-- post id, '' for the workspace), so it carries no foreign key; proposals of a
-- deleted subject simply expire.
CREATE TABLE assistant_proposals (
    id          TEXT        PRIMARY KEY,
    tenant_id   TEXT        NOT NULL REFERENCES tenants (id),
    scope_kind  TEXT        NOT NULL CHECK (scope_kind IN ('workspace', 'campaign', 'post')),
    scope_id    TEXT        NOT NULL DEFAULT '',
    tool        TEXT        NOT NULL,
    args        JSONB       NOT NULL DEFAULT '{}',
    summary     TEXT        NOT NULL,
    options     JSONB       NOT NULL DEFAULT '[]',
    status      TEXT        NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'confirmed', 'dismissed', 'expired')),
    chosen      TEXT        NOT NULL DEFAULT '',
    created_by  TEXT        NOT NULL DEFAULT '',
    resolved_by TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ
);

-- The runner reads a scope's open proposals on every turn.
CREATE INDEX idx_assistant_proposals_open
    ON assistant_proposals (tenant_id, scope_kind, scope_id, created_at DESC)
    WHERE status = 'pending';
