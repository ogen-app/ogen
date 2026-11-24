-- What sets the brand apart: one statement and the differences that back it.
-- A singleton like the look; one row per tenant.
CREATE TABLE brand_positioning (
    id              TEXT        PRIMARY KEY,
    tenant_id       TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    statement       TEXT        NOT NULL DEFAULT '',
    differentiators JSONB       NOT NULL DEFAULT '[]',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_brand_positioning_tenant ON brand_positioning (tenant_id);
