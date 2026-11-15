-- The member who last changed a post or an asset, so an offer or an undo that
-- finds it changed can say who did. NULL until a member edits it; a job's
-- write leaves it as it was. No FK: a member removed later still named the
-- change, and the reader falls back to saying nobody.
ALTER TABLE posts ADD COLUMN updated_by TEXT NULL;
ALTER TABLE assets ADD COLUMN updated_by TEXT NULL;
