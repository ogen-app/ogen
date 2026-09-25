-- CON-315: the workspace Ideas backlog. A one-line idea is either workspace-wide
-- (campaign_id NULL) or attached to one campaign, and carries a triage verdict
-- (yes / later / no, NULL = waiting in the inbox).
--
-- The verdict invariants live here as CHECKs, not only in the handler:
--   * remind_at exists exactly when verdict = 'later';
--   * decided_at exists exactly when a verdict is set.
-- A "woken" idea (later + remind_at <= now()) stays 'later' — waking is derived
-- at read time, nothing ever rewrites it.
--
-- Unlike post_notes, the user FKs are ON DELETE SET NULL: the backlog is shared,
-- so removing a member must not delete their ideas. Users are hard-deleted, so
-- created_by_name snapshots the author at capture time and survives that.
-- tenant_id cascades so a tenant hard-delete takes the backlog with it.
CREATE TABLE ideas (
    id              TEXT        PRIMARY KEY,
    tenant_id       TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    title           TEXT        NOT NULL,
    note            TEXT        NOT NULL DEFAULT '',
    campaign_id     TEXT        NULL REFERENCES campaigns (id) ON DELETE SET NULL,
    verdict         TEXT        NULL CHECK (verdict IN ('yes', 'later', 'no')),
    remind_at       TIMESTAMPTZ NULL,
    decided_at      TIMESTAMPTZ NULL,
    decided_by      TEXT        NULL REFERENCES users (id) ON DELETE SET NULL,
    created_by      TEXT        NULL REFERENCES users (id) ON DELETE SET NULL,
    created_by_name TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ideas_remind_only_for_later
        CHECK ((verdict IS NOT DISTINCT FROM 'later') = (remind_at IS NOT NULL)),
    CONSTRAINT ideas_decision_consistent
        CHECK ((verdict IS NULL) = (decided_at IS NULL))
);

CREATE INDEX idx_ideas_tenant_created  ON ideas (tenant_id, created_at);
CREATE INDEX idx_ideas_tenant_campaign ON ideas (tenant_id, campaign_id);
