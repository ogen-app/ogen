DROP TRIGGER IF EXISTS posts_touch_campaign_upd ON posts;
DROP TRIGGER IF EXISTS posts_touch_campaign_ins_del ON posts;
DROP FUNCTION IF EXISTS posts_touch_campaign();
DROP FUNCTION IF EXISTS campaigns_touch_posts_changed(TEXT);
ALTER TABLE campaigns DROP COLUMN posts_changed_at;
