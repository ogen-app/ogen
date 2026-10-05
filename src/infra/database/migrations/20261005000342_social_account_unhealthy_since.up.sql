-- Start of the account's current unhealthy episode, set by the connection-health
-- sweep the first time it classifies the account as expiring_soon or
-- action_required and cleared once Zernio reports it healthy again. The
-- owner-notification dedupe keys on it, so an episode notifies once per stage
-- no matter how often the reported token expiry moves.
ALTER TABLE social_accounts
    ADD COLUMN unhealthy_since TIMESTAMPTZ;
