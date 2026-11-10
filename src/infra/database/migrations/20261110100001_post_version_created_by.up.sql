-- created_by is the person whose action saved a post version, so "who
-- changed this post?" has an answer: the editor, the one the assistant worked
-- for, the one who submitted it. creator still says user, assistant or system.
-- Versions saved before it stay NULL; like post_logs.actor it is a user id
-- with no foreign key, so the history outlives a removed member.
ALTER TABLE post_versions
    ADD COLUMN created_by TEXT;
