-- CON-353: when a campaign's posts last changed in a way its Figma board shows.
--
-- Posts are hard-deleted, so the time of a deletion can't be read back from
-- posts; a trigger records it on the campaign instead. Every write path (REST,
-- assistant tools, content plan, jobs, cascades) goes through it.
ALTER TABLE campaigns ADD COLUMN posts_changed_at TIMESTAMPTZ NULL;

UPDATE campaigns AS c
SET posts_changed_at = p.changed
FROM (SELECT campaign_id, max(updated_at) AS changed FROM posts GROUP BY campaign_id) AS p
WHERE p.campaign_id = c.id;

-- clock_timestamp(), not now(): a long transaction must not stamp a change
-- with its start time, which could fall before a board synced in between.
-- GREATEST keeps the value from moving back when transactions commit out of
-- order.
CREATE FUNCTION campaigns_touch_posts_changed(campaign TEXT) RETURNS void
    LANGUAGE sql AS $$
    UPDATE campaigns
    SET posts_changed_at = GREATEST(posts_changed_at, clock_timestamp())
    WHERE id = campaign;
$$;

CREATE FUNCTION posts_touch_campaign() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        PERFORM campaigns_touch_posts_changed(OLD.campaign_id);
    END IF;
    IF TG_OP = 'INSERT'
        OR (TG_OP = 'UPDATE' AND NEW.campaign_id IS DISTINCT FROM OLD.campaign_id) THEN
        PERFORM campaigns_touch_posts_changed(NEW.campaign_id);
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER posts_touch_campaign_ins_del
    AFTER INSERT OR DELETE ON posts
    FOR EACH ROW
    EXECUTE FUNCTION posts_touch_campaign();

-- Only the fields a board shows: where a post sits (campaign, schedule), its
-- frame size (platform, post type), its label (title) and whether it can
-- still take media (status).
CREATE TRIGGER posts_touch_campaign_upd
    AFTER UPDATE OF campaign_id, scheduled_at, platform_id, platform_post_type, title, status ON posts
    FOR EACH ROW
    WHEN (OLD.campaign_id IS DISTINCT FROM NEW.campaign_id
        OR OLD.scheduled_at IS DISTINCT FROM NEW.scheduled_at
        OR OLD.platform_id IS DISTINCT FROM NEW.platform_id
        OR OLD.platform_post_type IS DISTINCT FROM NEW.platform_post_type
        OR OLD.title IS DISTINCT FROM NEW.title
        OR OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION posts_touch_campaign();
