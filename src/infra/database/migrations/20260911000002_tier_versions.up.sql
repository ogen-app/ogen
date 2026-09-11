-- CON-243: versioned tier entitlements. Make the price + full entitlement set an
-- immutable, versioned artifact hanging off the CON-208 tenant_tiers row (the
-- "plan"), that a tenant is assigned to over a time range with point-in-time
-- resolution. Three new GLOBAL operator tables + an immutability trigger + a
-- btree_gist no-overlap exclusion on the assignment history.
--
-- btree_gist provides the '=' GiST operator class needed to combine the scalar
-- tenant_id with the range-overlap (&&) operator in the assignment exclusion
-- constraint. It is the repo's first use of gist / a trigger — see the PRD.
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- Immutable, versioned snapshot of one tier's pricing + entitlements. status
-- moves draft -> active -> retired; a published (non-draft) row is frozen by the
-- trigger below except for the one-way active -> retired transition.
CREATE TABLE tenant_tier_versions (
    id            TEXT        PRIMARY KEY,
    tier_id       TEXT        NOT NULL REFERENCES tenant_tiers (id) ON DELETE RESTRICT,
    version       INTEGER     NOT NULL CHECK (version >= 1),
    status        TEXT        NOT NULL DEFAULT 'draft'
                              CHECK (status IN ('draft', 'active', 'retired')),
    purchasable   BOOLEAN     NOT NULL DEFAULT false,
    change_reason TEXT        NOT NULL DEFAULT '',
    entitlements  JSONB       NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at  TIMESTAMPTZ,
    retired_at    TIMESTAMPTZ,
    UNIQUE (tier_id, version)
);
CREATE INDEX idx_tenant_tier_versions_tier_status ON tenant_tier_versions (tier_id, status);

-- Price rows per version. NET (VAT-exclusive) minor units; gross is resolved at
-- billing/display time from the member state, so a VAT change is not our price
-- change. A null country_code row is the default for its currency/interval.
CREATE TABLE tenant_tier_version_prices (
    id               TEXT   PRIMARY KEY,
    tier_version_id  TEXT   NOT NULL REFERENCES tenant_tier_versions (id) ON DELETE CASCADE,
    currency         TEXT   NOT NULL,
    billing_interval TEXT   NOT NULL CHECK (billing_interval IN ('month', 'year')),
    net_minor        BIGINT NOT NULL CHECK (net_minor >= 0),
    country_code     TEXT,
    UNIQUE NULLS NOT DISTINCT (tier_version_id, currency, billing_interval, country_code)
);

-- Append-only tenant -> version history. The gist exclusion makes two overlapping
-- validity ranges for one tenant impossible, so point-in-time resolution can
-- never see two versions at once. An open-ended range (upper = infinity) is the
-- tenant's current assignment.
CREATE TABLE tenant_tier_assignments (
    id              TEXT        PRIMARY KEY,
    tenant_id       TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    tier_version_id TEXT        NOT NULL REFERENCES tenant_tier_versions (id) ON DELETE RESTRICT,
    valid           TSTZRANGE   NOT NULL,
    reason          TEXT        NOT NULL CHECK (reason IN
                        ('signup', 'migration_accepted', 'migration_lapsed', 'grandfathered', 'upgrade', 'downgrade')),
    notice_id       TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    EXCLUDE USING gist (tenant_id WITH =, valid WITH &&)
);
CREATE INDEX idx_tenant_tier_assignments_version ON tenant_tier_assignments (tier_version_id);

--bun:split

-- Immutability: a published (non-draft) version is frozen. The ONLY permitted
-- change is the one-way active -> retired transition (which also sets
-- retired_at). Everything else is a new version. Enforced in the DB because the
-- failure mode -- a silently rewritten price -- only surfaces when the audit
-- trail is the thing being questioned.
CREATE OR REPLACE FUNCTION tenant_tier_versions_immutable() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.status <> 'draft' THEN
            RAISE EXCEPTION 'tenant_tier_versions: cannot delete a % version (id=%)', OLD.status, OLD.id;
        END IF;
        RETURN OLD;
    END IF;
    -- UPDATE. Drafts are freely editable.
    IF OLD.status = 'draft' THEN
        RETURN NEW;
    END IF;
    -- Published: allow only active -> retired, changing nothing but status +
    -- retired_at.
    IF OLD.status = 'active' AND NEW.status = 'retired'
       AND NEW.id = OLD.id AND NEW.tier_id = OLD.tier_id AND NEW.version = OLD.version
       AND NEW.purchasable = OLD.purchasable AND NEW.change_reason = OLD.change_reason
       AND NEW.entitlements = OLD.entitlements
       AND NEW.published_at IS NOT DISTINCT FROM OLD.published_at THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'tenant_tier_versions: version % is % and immutable (only active->retired is allowed)', OLD.id, OLD.status;
END;
$$ LANGUAGE plpgsql;

--bun:split

CREATE TRIGGER trg_tenant_tier_versions_immutable
    BEFORE UPDATE OR DELETE ON tenant_tier_versions
    FOR EACH ROW EXECUTE FUNCTION tenant_tier_versions_immutable();

--bun:split

-- Price rows inherit their version's immutability: UPDATE/DELETE of a price
-- whose version is published is rejected. (INSERT is left to the seed/authoring
-- path, which adds prices while the version is still a draft.)
CREATE OR REPLACE FUNCTION tenant_tier_version_prices_immutable() RETURNS trigger AS $$
DECLARE
    parent_status TEXT;
BEGIN
    SELECT status INTO parent_status FROM tenant_tier_versions
        WHERE id = COALESCE(OLD.tier_version_id, NEW.tier_version_id);
    IF parent_status IS DISTINCT FROM 'draft' THEN
        RAISE EXCEPTION 'tenant_tier_version_prices: price rows of a published version are immutable (version=%)',
            COALESCE(OLD.tier_version_id, NEW.tier_version_id);
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

--bun:split

CREATE TRIGGER trg_tenant_tier_version_prices_immutable
    BEFORE UPDATE OR DELETE ON tenant_tier_version_prices
    FOR EACH ROW EXECUTE FUNCTION tenant_tier_version_prices_immutable();

--bun:split

-- Seed the commercial tiers (the 'default' tier already exists from CON-208).
INSERT INTO tenant_tiers (id, name, description) VALUES
    ('trial', 'Trial', 'Personal use — taste everything core, run nothing for long.'),
    ('pro',   'Pro',   'Personal brand — everything one brand needs, you plus two teammates.'),
    ('max',   'Max',   'Teams & agencies — full access, high ceilings.');

-- Seed v1 for each tier. Values come from the "Ogen — Workspace tiers: feature
-- distribution" doc (2026-08-19). A null numeric entitlement = unlimited.
--
--  * default-v1 is INTERNAL (purchasable=false) with an all-unlimited set, so
--    every existing tenant (all on the 'default' tier) keeps today's no-limits
--    behaviour once the boot backfill assigns them here.
--  * trial-v1 is the free public tier (active + purchasable).
--  * pro-v1 / max-v1 carry the doc's PROPOSED numbers but stay DRAFT until
--    pricing is decided and they are published via Harbor (CON-294) — so they are
--    not yet shown on the public pricing page (which lists active+purchasable).
INSERT INTO tenant_tier_versions (id, tier_id, version, status, purchasable, change_reason, entitlements, published_at) VALUES
    ('ttv-default-v1', 'default', 1, 'active', false, 'Initial internal version (grandfathers existing workspaces at no limits).',
        '{"workspaces":null,"team_seats":null,"connected_accounts":null,"active_campaigns":null,"all_campaign_types":true,"custom_campaign_types":true,"plan_runs_per_month":null,"assistant_multiplier":1,"quality_reviews_per_post":null,"posts_total":null,"media_storage_bytes":null,"content_bank_assets":null,"web_page_imports":null,"multiple_accounts_per_platform":true,"semantic_grounding":true}'::jsonb,
        now()),
    ('ttv-trial-v1', 'trial', 1, 'active', true, 'Initial published version.',
        '{"workspaces":1,"team_seats":1,"connected_accounts":2,"active_campaigns":1,"all_campaign_types":false,"custom_campaign_types":false,"plan_runs_per_month":3,"assistant_multiplier":1,"quality_reviews_per_post":1,"posts_total":15,"media_storage_bytes":104857600,"content_bank_assets":10,"web_page_imports":3,"multiple_accounts_per_platform":false,"semantic_grounding":true}'::jsonb,
        now()),
    ('ttv-pro-v1', 'pro', 1, 'draft', true, '',
        '{"workspaces":1,"team_seats":3,"connected_accounts":6,"active_campaigns":null,"all_campaign_types":true,"custom_campaign_types":false,"plan_runs_per_month":null,"assistant_multiplier":5,"quality_reviews_per_post":5,"posts_total":null,"media_storage_bytes":1073741824,"content_bank_assets":null,"web_page_imports":null,"multiple_accounts_per_platform":false,"semantic_grounding":true}'::jsonb,
        NULL),
    ('ttv-max-v1', 'max', 1, 'draft', true, '',
        '{"workspaces":5,"team_seats":null,"connected_accounts":30,"active_campaigns":null,"all_campaign_types":true,"custom_campaign_types":true,"plan_runs_per_month":null,"assistant_multiplier":20,"quality_reviews_per_post":10,"posts_total":null,"media_storage_bytes":10737418240,"content_bank_assets":null,"web_page_imports":null,"multiple_accounts_per_platform":true,"semantic_grounding":true}'::jsonb,
        NULL);

-- Trial is free (the doc's only decided price).
INSERT INTO tenant_tier_version_prices (id, tier_version_id, currency, billing_interval, net_minor, country_code) VALUES
    ('ttvp-trial-v1-eur-m', 'ttv-trial-v1', 'EUR', 'month', 0, NULL);
