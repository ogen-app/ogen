DROP TABLE IF EXISTS login_alert_tokens;
DROP TABLE IF EXISTS account_known_devices;
ALTER TABLE accounts DROP COLUMN IF EXISTS devices_enrolled_at;
