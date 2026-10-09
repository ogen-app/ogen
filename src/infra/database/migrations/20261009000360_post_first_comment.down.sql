UPDATE platforms
    SET text_constraints = text_constraints - 'max_first_comment_chars' - 'first_comment_per_post_type';

ALTER TABLE posts
    DROP COLUMN IF EXISTS first_comment_error,
    DROP COLUMN IF EXISTS first_comment_posted_at,
    DROP COLUMN IF EXISTS first_comment_id,
    DROP COLUMN IF EXISTS first_comment_status,
    DROP COLUMN IF EXISTS first_comment_delay_minutes,
    DROP COLUMN IF EXISTS first_comment;
