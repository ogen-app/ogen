-- A tier has at most one active purchasable version: the one the pricing page,
-- checkout and the variant mapping all treat as "on sale". Publishing a second
-- one must retire (supersede) the first. Drafts and retired versions are free.
--
-- Existing violations are not repaired here: which version to keep selling is
-- an operator decision. Retire the extra versions in Harbor, then redeploy.

DO $$
DECLARE
    clash TEXT;
BEGIN
    SELECT string_agg(format('%s (v%s)', tier_id, versions), ', ')
      INTO clash
      FROM (SELECT tier_id, string_agg(version::text, ', v' ORDER BY version) AS versions
              FROM tenant_tier_versions
             WHERE status = 'active' AND purchasable
             GROUP BY tier_id
            HAVING count(*) > 1) t;
    IF clash IS NOT NULL THEN
        RAISE EXCEPTION 'tenant_tier_versions: tiers with more than one active purchasable version: %; retire the extras before migrating', clash;
    END IF;
END
$$;

CREATE UNIQUE INDEX uq_tenant_tier_versions_active_purchasable
    ON tenant_tier_versions (tier_id)
    WHERE status = 'active' AND purchasable;
