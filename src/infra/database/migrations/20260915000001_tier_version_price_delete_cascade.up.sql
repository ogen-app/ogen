-- CON-297: allow deleting a DRAFT tier version that has price rows.
--
-- tenant_tier_versions permits deleting a draft (the immutability trigger), and
-- tenant_tier_version_prices has ON DELETE CASCADE — so deleting a draft version
-- cascades into its price rows. But the price-immutability trigger looked up the
-- parent version's status on DELETE and, finding the parent already gone during
-- the cascade (status NULL, which "IS DISTINCT FROM 'draft'"), wrongly rejected
-- the delete with "price rows of a published version are immutable". This blocked
-- both DeleteTierVersion (CON-297) and the leftover-draft path of DeleteTier.
--
-- Fix: on DELETE, reject only when the parent version still EXISTS and is
-- published. A missing parent means the delete is a cascade from the version's
-- own deletion, which tenant_tier_versions only permits for drafts — so it is
-- safe (mirrors the tenant_tier_assignments append-only trigger's cascade
-- carve-out). INSERT/UPDATE behaviour is unchanged.
CREATE OR REPLACE FUNCTION tenant_tier_version_prices_immutable() RETURNS trigger AS $$
DECLARE
    old_status TEXT;
    new_status TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        SELECT status INTO old_status FROM tenant_tier_versions WHERE id = OLD.tier_version_id;
        IF FOUND AND old_status <> 'draft' THEN
            RAISE EXCEPTION 'tenant_tier_version_prices: price rows of a published version are immutable (version=%)', OLD.tier_version_id;
        END IF;
        RETURN OLD;
    END IF;
    -- INSERT or UPDATE: the OLD parent (UPDATE only) and the NEW parent must be drafts.
    IF TG_OP = 'UPDATE' THEN
        SELECT status INTO old_status FROM tenant_tier_versions WHERE id = OLD.tier_version_id;
        IF old_status IS DISTINCT FROM 'draft' THEN
            RAISE EXCEPTION 'tenant_tier_version_prices: price rows of a published version are immutable (version=%)', OLD.tier_version_id;
        END IF;
    END IF;
    SELECT status INTO new_status FROM tenant_tier_versions WHERE id = NEW.tier_version_id;
    IF new_status IS DISTINCT FROM 'draft' THEN
        RAISE EXCEPTION 'tenant_tier_version_prices: cannot add or move a price to a published version (version=%)', NEW.tier_version_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
