-- A saved view is a named address of a filterable list: the list's own query
-- string, canonical, without paging. surface names the list; the server's
-- surface registry validates it, so a new list needs no migration.
--
-- created_by goes NULL when the member is removed: their private views are
-- deleted first, in the same transaction, and their shared views stay.
CREATE TABLE IF NOT EXISTS saved_views (
    id            TEXT        PRIMARY KEY,
    tenant_id     TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    surface       TEXT        NOT NULL,
    name          TEXT        NOT NULL,
    query         TEXT        NOT NULL DEFAULT '',
    query_version INTEGER     NOT NULL,
    shared        BOOLEAN     NOT NULL DEFAULT false,
    created_by    TEXT        NULL REFERENCES users (id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_saved_views_tenant_surface ON saved_views (tenant_id, surface, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_saved_views_created_by ON saved_views (created_by) WHERE created_by IS NOT NULL;
