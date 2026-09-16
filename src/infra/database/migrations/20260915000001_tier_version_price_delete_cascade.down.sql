-- Restore the original price-immutability function (CON-243, migration
-- 20260911000002): reject any non-INSERT touching a price row whose parent
-- version is not a draft. Note this reinstates the latent bug where deleting a
-- draft version that HAS price rows fails on the cascade.
CREATE OR REPLACE FUNCTION tenant_tier_version_prices_immutable() RETURNS trigger AS $$
DECLARE
    old_status TEXT;
    new_status TEXT;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        SELECT status INTO old_status FROM tenant_tier_versions WHERE id = OLD.tier_version_id;
        IF old_status IS DISTINCT FROM 'draft' THEN
            RAISE EXCEPTION 'tenant_tier_version_prices: price rows of a published version are immutable (version=%)', OLD.tier_version_id;
        END IF;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        SELECT status INTO new_status FROM tenant_tier_versions WHERE id = NEW.tier_version_id;
        IF new_status IS DISTINCT FROM 'draft' THEN
            RAISE EXCEPTION 'tenant_tier_version_prices: cannot add or move a price to a published version (version=%)', NEW.tier_version_id;
        END IF;
        RETURN NEW;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
