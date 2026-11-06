-- One conversation store for the universal assistant, keyed by the thread's
-- scope (the workspace, a campaign or a post) instead of one table per subject.
-- A user turn records what the user was looking at (view_context), so "those"
-- in a later turn still resolves and history replays with its screen. scope_id
-- is polymorphic ('' for the workspace), so it carries no foreign key.
CREATE TABLE assistant_messages (
    id           TEXT        PRIMARY KEY,
    tenant_id    TEXT        NOT NULL REFERENCES tenants (id),
    scope_kind   TEXT        NOT NULL CHECK (scope_kind IN ('workspace', 'campaign', 'post')),
    scope_id     TEXT        NOT NULL DEFAULT '',
    role         TEXT        NOT NULL CHECK (role IN ('user', 'model')),
    content      TEXT        NOT NULL,
    view_context JSONB,
    created_by   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_assistant_messages_thread
    ON assistant_messages (tenant_id, scope_kind, scope_id, created_at DESC);
