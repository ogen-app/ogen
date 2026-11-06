-- Reversible changes the assistant made, with the state before and after, so
-- the user can undo one from its result card. Undo restores before_state only
-- while the entity still matches after_state, so it never overwrites a later edit.
-- entity_id is polymorphic, so it carries no foreign key.
CREATE TABLE assistant_actions (
    id           TEXT        PRIMARY KEY,
    tenant_id    TEXT        NOT NULL REFERENCES tenants (id),
    scope_kind   TEXT        NOT NULL CHECK (scope_kind IN ('workspace', 'campaign', 'post')),
    scope_id     TEXT        NOT NULL DEFAULT '',
    kind         TEXT        NOT NULL,
    entity_id    TEXT        NOT NULL,
    before_state JSONB       NOT NULL,
    after_state  JSONB       NOT NULL,
    created_by   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    undone_at    TIMESTAMPTZ,
    undone_by    TEXT        NOT NULL DEFAULT ''
);

CREATE INDEX idx_assistant_actions_scope
    ON assistant_actions (tenant_id, scope_kind, scope_id, created_at DESC);
