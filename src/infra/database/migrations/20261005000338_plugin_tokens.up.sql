-- Plugin connections: a design-tool plugin (Figma first) pushes rendered
-- frames into a workspace's content bank, authenticated by a long-lived,
-- revocable plugin token obtained once through a pairing handshake.

-- plugin_tokens is an auth credential, looked up by hash BEFORE the tenant is
-- known (that lookup is how the tenant is discovered), so like sessions it
-- carries a plain tenant_id rather than the tenant-scoping hook. Only the
-- sha256 of the token is stored. user_id is the membership the token acts as:
-- removing the member cascades its tokens away, so a re-invited member never
-- inherits an old connection.
CREATE TABLE plugin_tokens (
    id           TEXT        PRIMARY KEY,
    tenant_id    TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    account_id   TEXT        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    user_id      TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    client       TEXT        NOT NULL CHECK (client IN ('figma')),
    label        TEXT        NOT NULL,
    token_hash   TEXT        NOT NULL UNIQUE,
    last_used_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ
);

-- The connections list reads a workspace's live tokens, optionally per member.
CREATE INDEX idx_plugin_tokens_tenant_user ON plugin_tokens (tenant_id, user_id)
    WHERE revoked_at IS NULL;

-- plugin_pairings is the short-lived handshake behind a plugin's "Connect"
-- button. The plugin holds the read key (the only way to collect the token);
-- the write key travels through the browser to the approval page and can only
-- preview, approve or deny. Both are stored as sha256 hashes. Approval seals
-- the plaintext token into sealed_token (envelope-encrypted) until the plugin
-- collects it, which deletes the row. tenant_id is unknown until approval.
-- Readers treat a row past expires_at as gone; a periodic job sweeps them.
CREATE TABLE plugin_pairings (
    id             TEXT        PRIMARY KEY,
    client         TEXT        NOT NULL CHECK (client IN ('figma')),
    client_label   TEXT        NOT NULL,
    read_key_hash  TEXT        NOT NULL UNIQUE,
    write_key_hash TEXT        NOT NULL UNIQUE,
    status         TEXT        NOT NULL DEFAULT 'pending'
                       CHECK (status IN ('pending', 'approved', 'denied')),
    tenant_id      TEXT        REFERENCES tenants (id) ON DELETE CASCADE,
    token_id       TEXT        REFERENCES plugin_tokens (id) ON DELETE CASCADE,
    sealed_token   BYTEA,
    created_ip     TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,
    CONSTRAINT plugin_pairings_approved_has_token
        CHECK ((status = 'approved') = (token_id IS NOT NULL))
);

-- The expiry sweep scans by expires_at.
CREATE INDEX idx_plugin_pairings_expires_at ON plugin_pairings (expires_at);

-- Asset provenance: where an asset came from beyond a plain upload. origin is
-- '' for uploads and everything that predates it; origin_ref carries the
-- origin-specific pointer (for Figma: node id, node name, file name and, from
-- private builds only, the file key).
ALTER TABLE assets
    ADD COLUMN origin     TEXT  NOT NULL DEFAULT '' CHECK (origin IN ('', 'figma')),
    ADD COLUMN origin_ref JSONB;
