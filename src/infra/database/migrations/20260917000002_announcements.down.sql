-- Reverse of 20260917000001_announcements. Drop in FK-dependency order (the
-- children reference announcements + the classification/user/tenant tables).
DROP TABLE IF EXISTS announcement_interactions;
DROP TABLE IF EXISTS announcement_target_tiers;
DROP TABLE IF EXISTS announcement_target_groups;
DROP TABLE IF EXISTS announcements;
