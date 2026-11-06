-- Findings the user dismissed ("that's fine, leave it"), so the next check
-- of the same thread doesn't raise them again. A finding's key changes when
-- its posts do, so a dismissal covers exactly what the user saw.
CREATE TABLE assistant_dismissals (
    tenant_id   TEXT        NOT NULL REFERENCES tenants (id),
    scope_kind  TEXT        NOT NULL CHECK (scope_kind IN ('workspace', 'campaign', 'post')),
    scope_id    TEXT        NOT NULL DEFAULT '',
    finding_key TEXT        NOT NULL,
    created_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, scope_kind, scope_id, finding_key)
);
