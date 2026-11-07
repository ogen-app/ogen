-- "planned" is a post with its theses drafted and no body yet. Until now the
-- content plan saved those as drafts with an empty body and a draft_thesis
-- note; they are what planned means, so they move over. A draft with any
-- body, or without theses, stays a draft. posts.status is plain text, so the
-- status itself needs no schema change.
--
-- A blank body is what Go's strings.TrimSpace empties: the Unicode White_Space
-- characters, not only ASCII whitespace.
--
-- Status changes made here are not written to post_logs: no one changed the
-- posts, their stage was renamed.
UPDATE posts p
SET status = 'planned'
WHERE p.status = 'draft'
  AND p.content ~ '^[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]*$'
  AND EXISTS (
      SELECT 1 FROM post_notes n
      WHERE n.post_id = p.id AND n.type = 'draft_thesis'
  );
