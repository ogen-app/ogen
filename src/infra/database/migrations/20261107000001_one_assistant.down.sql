-- Restores the per-subject conversation tables from the assistant's campaign
-- and post threads. The model configs the up migration removed are seeded
-- again from the catalog's defaults at boot.
CREATE TABLE campaign_assistant_messages (
    id          TEXT        PRIMARY KEY,
    campaign_id TEXT        NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    role        TEXT        NOT NULL CHECK (role IN ('user', 'model')),
    content     TEXT        NOT NULL,
    tenant_id   TEXT        NOT NULL REFERENCES tenants (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_campaign_assistant_messages_campaign_id ON campaign_assistant_messages (campaign_id);
CREATE INDEX idx_campaign_assistant_messages_tenant_id ON campaign_assistant_messages (tenant_id);

CREATE TABLE post_assistant_messages (
    id         TEXT        PRIMARY KEY,
    post_id    TEXT        NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
    role       TEXT        NOT NULL CHECK (role IN ('user', 'model')),
    content    TEXT        NOT NULL,
    tenant_id  TEXT        NOT NULL REFERENCES tenants (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_post_assistant_messages_post_id ON post_assistant_messages (post_id);
CREATE INDEX idx_post_assistant_messages_tenant_id ON post_assistant_messages (tenant_id);

INSERT INTO campaign_assistant_messages (id, campaign_id, role, content, tenant_id, created_at)
SELECT m.id, m.scope_id, m.role, m.content, m.tenant_id, m.created_at
FROM assistant_messages m
JOIN campaigns c ON c.id = m.scope_id
WHERE m.scope_kind = 'campaign';

INSERT INTO post_assistant_messages (id, post_id, role, content, tenant_id, created_at)
SELECT m.id, m.scope_id, m.role, m.content, m.tenant_id, m.created_at
FROM assistant_messages m
JOIN posts p ON p.id = m.scope_id
WHERE m.scope_kind = 'post';
