DROP INDEX IF EXISTS idx_assistant_proposals_replaces;

ALTER TABLE assistant_proposals
    DROP COLUMN IF EXISTS replaces,
    DROP COLUMN IF EXISTS retry_of;
