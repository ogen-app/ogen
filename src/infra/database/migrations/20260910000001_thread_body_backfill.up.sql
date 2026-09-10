-- CON-284 R2: the thread authoring model inverted. posts.content is now the
-- canonical thread body — the full text the author typed, with "---" delimiter
-- lines — and thread_segments is DERIVED from it by the server. Under R1, content
-- held only the root message (segment 0) while the ordered chain lived in
-- thread_segments. Rewrite content for any existing thread so it holds the whole
-- body, making content the single source of truth going forward.
--
-- The body is the segment contents joined by a blank-line-padded "---" rule — the
-- exact form SplitThread round-trips back into the same segments. thread_segments
-- is left untouched (it is re-derived on the next write). Idempotent: the join
-- reads the unchanged segments, so re-running reproduces identical content. The
-- composer UI never shipped under R1, so the real affected row count is ~0.
UPDATE posts AS p
SET content = sub.body
FROM (
    SELECT posts.id,
           string_agg(seg.value ->> 'content', E'\n\n---\n\n' ORDER BY seg.ord) AS body
    FROM posts,
         jsonb_array_elements(posts.thread_segments) WITH ORDINALITY AS seg(value, ord)
    WHERE posts.platform_post_type = 'thread'
      AND jsonb_array_length(posts.thread_segments) > 1
    GROUP BY posts.id
) AS sub
WHERE p.id = sub.id;
