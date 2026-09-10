-- CON-284 R2 rollback: restore R1's behaviour where a thread's content mirrors
-- only the root (segment 0). thread_segments was never modified by the up
-- migration, so the root is still available to reconstruct from.
UPDATE posts
SET content = thread_segments -> 0 ->> 'content'
WHERE platform_post_type = 'thread'
  AND jsonb_array_length(thread_segments) > 0;
