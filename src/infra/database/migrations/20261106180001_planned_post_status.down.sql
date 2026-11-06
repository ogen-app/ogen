-- Without the planned stage, a planned post is an empty draft again.
UPDATE posts SET status = 'draft' WHERE status = 'planned';
