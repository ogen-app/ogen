-- CON-360: a first comment posted under a post, with it or 1/3/5/10 minutes
-- after it goes live.
--
-- first_comment / first_comment_delay_minutes are authored on the post.
-- The other four columns are written by the publish workers only:
-- first_comment_status is NULL until the post goes live with a comment.
ALTER TABLE posts
    ADD COLUMN first_comment TEXT NULL,
    ADD COLUMN first_comment_delay_minutes SMALLINT NOT NULL DEFAULT 0
        CONSTRAINT posts_first_comment_delay_check CHECK (first_comment_delay_minutes IN (0, 1, 3, 5, 10)),
    ADD COLUMN first_comment_status TEXT NULL
        CONSTRAINT posts_first_comment_status_check
        CHECK (first_comment_status IN ('pending', 'delegated', 'posted', 'failed', 'skipped')),
    ADD COLUMN first_comment_id TEXT NULL,
    ADD COLUMN first_comment_posted_at TIMESTAMPTZ NULL,
    ADD COLUMN first_comment_error TEXT NULL;

-- Which platforms take a first comment, and how long it may be. Zernio
-- documents a first comment for these five; Threads (500) and YouTube (10,000)
-- limits are Zernio's, the others are the networks' own comment limits.
-- Stories take no comments, and LinkedIn articles are left out until verified.
UPDATE platforms SET text_constraints = text_constraints ||
    '{"max_first_comment_chars":8000,"first_comment_per_post_type":{"story":0}}'
    WHERE id = 'zBU1zqVICGfk';

UPDATE platforms SET text_constraints = text_constraints ||
    '{"max_first_comment_chars":2200,"first_comment_per_post_type":{"story":0}}'
    WHERE id = 'rzgpTkARLH0L';

UPDATE platforms SET text_constraints = text_constraints ||
    '{"max_first_comment_chars":1250,"first_comment_per_post_type":{"article":0}}'
    WHERE id = 'AXqWG7U2qnpt';

UPDATE platforms SET text_constraints = text_constraints ||
    '{"max_first_comment_chars":500}'
    WHERE id = 'pQ4yxT3SuE57';

UPDATE platforms SET text_constraints = text_constraints ||
    '{"max_first_comment_chars":10000}'
    WHERE id = '8S8bWQTG6qD';
