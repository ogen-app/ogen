-- Who may create a workspace. One row, edited from Harbor. The mode decides
-- self-registration only: tester invites work in both modes. Seeded open, so
-- deploying changes nothing.
CREATE TABLE platform_signup_settings (
    id                               TEXT        PRIMARY KEY CHECK (id = 'global'),
    mode                             TEXT        NOT NULL DEFAULT 'open' CHECK (mode IN ('open', 'invite_only')),
    -- While invite-only, how many live workspaces an account may own (0 = no cap).
    max_owned_workspaces_per_account INTEGER     NOT NULL DEFAULT 2 CHECK (max_owned_workspaces_per_account >= 0),
    updated_at                       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by                       TEXT        NOT NULL DEFAULT ''
);
INSERT INTO platform_signup_settings (id) VALUES ('global');

-- Operator-issued tester invites. Redeeming one creates a new complimentary
-- workspace. Only the token's hash is stored; expiry is derived from
-- expires_at, not a status.
CREATE TABLE tenant_invites (
    id                  TEXT        PRIMARY KEY,
    email               TEXT        NOT NULL,
    token_hash          TEXT        NOT NULL UNIQUE,
    -- NULL once the tier is deleted; a redemption then falls back to the default tier.
    tier_id             TEXT        REFERENCES tenant_tiers (id) ON DELETE SET NULL,
    message             TEXT,
    note                TEXT,
    created_by          TEXT        NOT NULL,
    status              TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'redeemed', 'revoked')),
    -- The lifetime the invite was issued with; a resend without a new one reuses it.
    ttl_hours           INTEGER     NOT NULL CHECK (ttl_hours BETWEEN 1 AND 720),
    expires_at          TIMESTAMPTZ NOT NULL,
    sent_count          INTEGER     NOT NULL DEFAULT 1,
    last_sent_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    redeemed_at         TIMESTAMPTZ,
    redeemed_tenant_id  TEXT        REFERENCES tenants (id) ON DELETE SET NULL,
    redeemed_account_id TEXT        REFERENCES accounts (id) ON DELETE SET NULL,
    revoked_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- At most one live invite per address.
CREATE UNIQUE INDEX uq_tenant_invites_pending_email ON tenant_invites (email) WHERE status = 'pending';
-- The operator list pages newest first.
CREATE INDEX idx_tenant_invites_created ON tenant_invites (created_at DESC, id DESC);
CREATE INDEX idx_tenant_invites_redeemed_tenant ON tenant_invites (redeemed_tenant_id) WHERE redeemed_tenant_id IS NOT NULL;

-- Cohorts are tenant groups; redemption copies them onto the new workspace.
CREATE TABLE tenant_invite_groups (
    invite_id TEXT NOT NULL REFERENCES tenant_invites (id) ON DELETE CASCADE,
    group_id  TEXT NOT NULL REFERENCES tenant_groups (id) ON DELETE CASCADE,
    PRIMARY KEY (invite_id, group_id)
);
CREATE INDEX idx_tenant_invite_groups_group ON tenant_invite_groups (group_id);

-- invite_id attributes a workspace to the invite it came from. complimentary
-- marks a tester's workspace: never billed, and the owner may switch tiers
-- without a checkout. A complimentary workspace is never behind the billing gate.
ALTER TABLE tenants
    ADD COLUMN invite_id     TEXT    REFERENCES tenant_invites (id) ON DELETE SET NULL,
    ADD COLUMN complimentary BOOLEAN NOT NULL DEFAULT false,
    ADD CONSTRAINT tenants_complimentary_not_billed CHECK (NOT (complimentary AND billing_required));

-- Tier moves of a complimentary workspace (redemption and the owner's own
-- switches) are recorded with reason 'comp'.
ALTER TABLE tenant_tier_assignments DROP CONSTRAINT tenant_tier_assignments_reason_check;
ALTER TABLE tenant_tier_assignments ADD CONSTRAINT tenant_tier_assignments_reason_check
    CHECK (reason IN ('signup', 'migration_accepted', 'migration_lapsed', 'grandfathered', 'upgrade', 'downgrade', 'operator_set', 'subscription_started', 'comp')) NOT VALID;
