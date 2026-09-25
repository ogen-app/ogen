-- CON-314: scope custom campaign types to their workspace.
--
-- campaigns_types was global reference data (CON-97 §5.2), which is right for
-- the seeded system types but let custom types created via the API leak across
-- workspaces. A nullable tenant_id splits the two: NULL = system type visible to
-- every workspace, set = custom type owned by that workspace. Phases inherit
-- ownership through their type.

ALTER TABLE campaigns_types ADD COLUMN tenant_id TEXT REFERENCES tenants (id);

-- Names were globally unique; they become unique per workspace and among system
-- types. Dropped before the backfill so a type shared by several workspaces can
-- be cloned under its own name.
ALTER TABLE campaigns_types DROP CONSTRAINT campaigns_types_name_key;

-- Backfill. Existing custom types have no recorded owner, so it is inferred from
-- the campaigns (soft-deleted ones included — they still hold the FK) using them:
--   * used by one workspace   -> owned by it;
--   * used by several         -> the workspace with the earliest campaign keeps
--                                the original, every other one gets a clone
--                                (same name, so the UI's slug lookup still hits)
--                                and its campaigns, post phases and phase plans
--                                are repointed to the clone;
--   * used by none            -> the default workspace (not deleted: that can't
--                                be undone).
--
-- The CON-166 integrity triggers would reject the repoint mid-way (a campaign
-- whose posts are phased is type-locked, and a post's phase must belong to its
-- campaign's current type), so they are off for the backfill. It keeps both
-- invariants: each workspace's campaigns and their post phases move together.
ALTER TABLE campaigns DISABLE TRIGGER campaigns_type_locked;
ALTER TABLE posts DISABLE TRIGGER posts_phase_matches_campaign_type_upd;

DO $$
DECLARE
    v_type      RECORD;
    v_owner     TEXT;
    v_other     TEXT;
    v_clone_id  TEXT;
    v_phase     RECORD;
    v_phase_id  TEXT;
BEGIN
    FOR v_type IN SELECT * FROM campaigns_types WHERE NOT is_system ORDER BY id LOOP
        SELECT c.tenant_id INTO v_owner
        FROM campaigns c
        WHERE c.campaign_type_id = v_type.id
        ORDER BY c.created_at, c.id
        LIMIT 1;

        UPDATE campaigns_types
        SET tenant_id = COALESCE(v_owner, 'default')
        WHERE id = v_type.id;

        FOR v_other IN
            SELECT DISTINCT c.tenant_id
            FROM campaigns c
            WHERE c.campaign_type_id = v_type.id AND c.tenant_id <> v_owner
            ORDER BY c.tenant_id
        LOOP
            v_clone_id := 'ct' || substr(md5(random()::text || clock_timestamp()::text || v_type.id || v_other), 1, 14);
            INSERT INTO campaigns_types (id, name, label, description, is_system, tenant_id, created_at, updated_at)
            VALUES (v_clone_id, v_type.name, v_type.label, v_type.description, FALSE, v_other, v_type.created_at, now());

            FOR v_phase IN SELECT * FROM campaigns_types_phases WHERE campaign_type_id = v_type.id LOOP
                v_phase_id := 'cp' || substr(md5(random()::text || clock_timestamp()::text || v_phase.id || v_other), 1, 14);
                INSERT INTO campaigns_types_phases (id, campaign_type_id, name, purpose, sequence, created_at, updated_at)
                VALUES (v_phase_id, v_clone_id, v_phase.name, v_phase.purpose, v_phase.sequence, v_phase.created_at, now());

                UPDATE posts p
                SET campaign_type_phase_id = v_phase_id
                FROM campaigns c
                WHERE p.campaign_id = c.id
                  AND c.tenant_id = v_other
                  AND c.campaign_type_id = v_type.id
                  AND p.campaign_type_phase_id = v_phase.id;

                UPDATE campaign_phase_windows w
                SET phase_id = v_phase_id
                FROM campaigns c
                WHERE w.campaign_id = c.id
                  AND c.tenant_id = v_other
                  AND c.campaign_type_id = v_type.id
                  AND w.phase_id = v_phase.id;
            END LOOP;

            UPDATE campaigns
            SET campaign_type_id = v_clone_id
            WHERE campaign_type_id = v_type.id AND tenant_id = v_other;
        END LOOP;
    END LOOP;
END;
$$;

ALTER TABLE posts ENABLE TRIGGER posts_phase_matches_campaign_type_upd;
ALTER TABLE campaigns ENABLE TRIGGER campaigns_type_locked;

-- A system type is exactly one with no owner.
ALTER TABLE campaigns_types
    ADD CONSTRAINT campaigns_types_owner_matches_system CHECK ((tenant_id IS NULL) = is_system);

CREATE UNIQUE INDEX campaigns_types_tenant_name_key ON campaigns_types (tenant_id, name) WHERE tenant_id IS NOT NULL;
CREATE UNIQUE INDEX campaigns_types_system_name_key ON campaigns_types (name) WHERE tenant_id IS NULL;
