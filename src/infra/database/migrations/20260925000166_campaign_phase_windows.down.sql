DROP TRIGGER IF EXISTS campaigns_type_locked ON campaigns;
DROP FUNCTION IF EXISTS campaigns_check_type_lock();
DROP TRIGGER IF EXISTS posts_phase_matches_campaign_type_upd ON posts;
DROP TRIGGER IF EXISTS posts_phase_matches_campaign_type_ins ON posts;
DROP FUNCTION IF EXISTS posts_check_phase_matches_campaign_type();
DROP TABLE IF EXISTS campaign_phase_windows;
