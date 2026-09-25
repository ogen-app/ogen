-- CON-166: campaign phase date plans, campaign-type lock, phase integrity.

-- 1. Manual phase plans. A campaign's phase windows are derived from its dates
--    by default (domain/campaignphase); rows here exist only for a user-edited
--    plan, stored whole (one row per phase of the campaign's type). Inclusive
--    calendar days. Deleting a phase from a custom type cascades its row away,
--    which invalidates the plan (read-time fallback to derived).
CREATE TABLE campaign_phase_windows (
    campaign_id TEXT        NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    phase_id    TEXT        NOT NULL REFERENCES campaigns_types_phases (id) ON DELETE CASCADE,
    tenant_id   TEXT        NOT NULL REFERENCES tenants (id),
    start_date  DATE        NOT NULL,
    end_date    DATE        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (campaign_id, phase_id),
    CHECK (end_date >= start_date)
);

CREATE INDEX idx_campaign_phase_windows_tenant_id ON campaign_phase_windows (tenant_id);

-- 2. Orphan repair. Switching a campaign's type used to leave its posts pointing
--    at the old type's phases, and a post write could name any type's phase.
--    Clear every phase reference that doesn't belong to the post's campaign's
--    type — before the lock below, so campaigns "locked" only by orphans aren't.
UPDATE posts p
SET campaign_type_phase_id = NULL
FROM campaigns c, campaigns_types_phases ph
WHERE p.campaign_id = c.id
  AND ph.id = p.campaign_type_phase_id
  AND ph.campaign_type_id <> c.campaign_type_id;

-- 3. Phase ownership: a post's phase must belong to its campaign's type. The
--    API checks this first to return a clean 400; this is the backstop for
--    every other write path. FOR SHARE on the campaign row serialises the
--    check against a concurrent campaign-type switch (which takes the row's
--    update lock), so the two can't interleave into an orphan.
CREATE FUNCTION posts_check_phase_matches_campaign_type() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_type TEXT;
BEGIN
    SELECT campaign_type_id INTO v_type FROM campaigns WHERE id = NEW.campaign_id FOR SHARE;
    IF NOT EXISTS (
        SELECT 1 FROM campaigns_types_phases
        WHERE id = NEW.campaign_type_phase_id AND campaign_type_id = v_type
    ) THEN
        RAISE EXCEPTION 'campaign_type_phase_id % does not belong to the campaign''s type', NEW.campaign_type_phase_id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'posts_phase_matches_campaign_type';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER posts_phase_matches_campaign_type_ins
    BEFORE INSERT ON posts
    FOR EACH ROW
    WHEN (NEW.campaign_type_phase_id IS NOT NULL)
    EXECUTE FUNCTION posts_check_phase_matches_campaign_type();

CREATE TRIGGER posts_phase_matches_campaign_type_upd
    BEFORE UPDATE OF campaign_type_phase_id, campaign_id ON posts
    FOR EACH ROW
    WHEN (NEW.campaign_type_phase_id IS NOT NULL
          AND (NEW.campaign_type_phase_id IS DISTINCT FROM OLD.campaign_type_phase_id
               OR NEW.campaign_id IS DISTINCT FROM OLD.campaign_id))
    EXECUTE FUNCTION posts_check_phase_matches_campaign_type();

-- 4. Campaign-type lock: once any of a campaign's posts is planned against a
--    phase, its type can't change (the phase references would orphan). The
--    API returns 409 campaign_type_locked first; this is the backstop.
CREATE FUNCTION campaigns_check_type_lock() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM posts
        WHERE campaign_id = NEW.id AND campaign_type_phase_id IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'campaign % has posts assigned to its type''s phases; its type is locked', NEW.id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'campaigns_type_locked';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER campaigns_type_locked
    BEFORE UPDATE OF campaign_type_id ON campaigns
    FOR EACH ROW
    WHEN (NEW.campaign_type_id IS DISTINCT FROM OLD.campaign_type_id)
    EXECUTE FUNCTION campaigns_check_type_lock();
