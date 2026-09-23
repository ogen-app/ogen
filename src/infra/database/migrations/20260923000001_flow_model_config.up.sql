-- CON-308: per-flow / per-tier model configuration. Maps a (tier, flow, slot)
-- to a concrete model id, so operators pick the foundation model each genkit
-- flow uses via Harbor (ModelConfigAdminService) with no deploy. tier_id NULL =
-- the global-default row (the required fallback used when a tenant's tier has no
-- override); a non-NULL tier_id overrides the default for that tier. Resolution
-- is `tier-override ?? global-default`.
--
-- GLOBAL operator table — NOT tenant-scoped — like tenant_tiers / platforms /
-- announcements. The flow/slot catalog and the model catalog + prices stay in
-- code (the CON-86 vendor registry); only the assignment lives here.
--
-- No seed here: the boot-time reconcile (CON-308 §10.2) inserts any missing
-- global-default row from the current config fields (MODEL_ID / PLANNING_MODEL_ID
-- / QUALITY_MODEL_ID / EMBED_MODEL), so an operator's existing env override
-- carries over exactly and day-one behaviour is byte-identical.
CREATE TABLE flow_model_config (
    id         TEXT        PRIMARY KEY,
    tier_id    TEXT        REFERENCES tenant_tiers (id) ON DELETE CASCADE,
    flow_key   TEXT        NOT NULL,
    slot_key   TEXT        NOT NULL,
    model_id   TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT        NOT NULL DEFAULT ''
);

-- One row per (scope, flow, slot). NULL tier_id (the global scope) is coalesced
-- to '' so the unique constraint treats "global" as a single distinct scope —
-- two NULLs would otherwise never collide. tier ids are Sqids, never '', so
-- there is no clash between a real tier and the global sentinel.
CREATE UNIQUE INDEX ux_flow_model_config
    ON flow_model_config (COALESCE(tier_id, ''), flow_key, slot_key);
