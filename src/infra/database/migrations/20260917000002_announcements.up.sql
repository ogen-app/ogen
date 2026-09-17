-- CON-230: operator-authored informational announcements (banners) shown to
-- tenants, with per-user click/dismiss tracking. Authored by Harbor over the
-- internal gRPC surface; delivered to the tenant app over /api/announcements.
--
-- Unlike the CON-242 notification inbox, an announcement is a SINGLE global row
-- (one banner shown to many tenants), so `announcements` is a GLOBAL operator
-- table — NOT tenant-scoped — like tenants / tenant_tiers / tenant_groups.
-- Targeting is expressed as target_all + the two join tables: a tenant matches
-- when target_all is true, OR its tier is listed, OR any of its groups is
-- listed.
--
-- Engagement is tracked per USER (one row per (announcement, user)); the
-- interaction row carries a denormalised tenant_id so the operator stats roll up
-- to unique-tenant counts. It is likewise NOT tenant-scoped: the Harbor stats
-- reads are cross-tenant aggregates, so the row carries explicit user_id +
-- tenant_id columns rather than relying on the app-level tenant hook.

CREATE TABLE announcements (
    id           TEXT        PRIMARY KEY,
    title        TEXT        NOT NULL,
    body         TEXT        NOT NULL,
    image_url    TEXT        NOT NULL DEFAULT '',
    image_alt    TEXT        NOT NULL DEFAULT '',
    cta_label    TEXT        NOT NULL DEFAULT '',
    cta_url      TEXT        NOT NULL DEFAULT '',
    target_all   BOOLEAN     NOT NULL DEFAULT false,
    status       TEXT        NOT NULL DEFAULT 'draft'
                             CHECK (status IN ('draft', 'published', 'archived')),
    starts_at    TIMESTAMPTZ,
    ends_at      TIMESTAMPTZ,
    published_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Delivery scans published, in-window rows newest-first; a partial index over
-- just the published rows keeps that hot path off the draft/archived history.
CREATE INDEX idx_announcements_published ON announcements (published_at DESC)
    WHERE status = 'published';

-- Targeting join tables. FK CASCADE so deleting a tier/group in Harbor cleans up
-- its targeting rows (an announcement can then silently narrow — documented).
CREATE TABLE announcement_target_groups (
    announcement_id TEXT NOT NULL REFERENCES announcements (id)  ON DELETE CASCADE,
    group_id        TEXT NOT NULL REFERENCES tenant_groups (id)  ON DELETE CASCADE,
    PRIMARY KEY (announcement_id, group_id)
);
-- Reverse "which announcements target this group" lookups (the composite PK's
-- leading column already serves per-announcement lookups).
CREATE INDEX idx_announcement_target_groups_group ON announcement_target_groups (group_id);

CREATE TABLE announcement_target_tiers (
    announcement_id TEXT NOT NULL REFERENCES announcements (id) ON DELETE CASCADE,
    tier_id         TEXT NOT NULL REFERENCES tenant_tiers (id)  ON DELETE CASCADE,
    PRIMARY KEY (announcement_id, tier_id)
);
CREATE INDEX idx_announcement_target_tiers_tier ON announcement_target_tiers (tier_id);

-- Per-user engagement. One row per (announcement, user), upserted on first
-- click / dismiss. tenant_id is denormalised for the per-tenant rollup in the
-- operator stats. NOT tenant-scoped (see file header). The UNIQUE
-- (announcement_id, user_id) index doubles as the per-announcement stats index
-- (leading column) and the delivery "did this user dismiss it" lookup.
CREATE TABLE announcement_interactions (
    id              TEXT        PRIMARY KEY,
    announcement_id TEXT        NOT NULL REFERENCES announcements (id) ON DELETE CASCADE,
    user_id         TEXT        NOT NULL REFERENCES users (id)         ON DELETE CASCADE,
    tenant_id       TEXT        NOT NULL REFERENCES tenants (id)       ON DELETE CASCADE,
    clicked_at      TIMESTAMPTZ,
    dismissed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (announcement_id, user_id)
);
