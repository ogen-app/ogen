-- Whether a generated post drew on the campaign's own sources ('grounded') or
-- on general knowledge ('exploratory'), recorded when the post is generated so
-- the label outlives the chat turn. NULL for posts written by hand or before
-- this was recorded.
ALTER TABLE posts
    ADD COLUMN grounding_mode TEXT CHECK (grounding_mode IN ('grounded', 'exploratory'));
