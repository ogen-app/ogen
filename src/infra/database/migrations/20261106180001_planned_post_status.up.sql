-- "planned" is a post with its theses drafted and no body yet. Until now the
-- content plan saved those as drafts with an empty body and a draft_thesis
-- note; they are what planned means, so they move over. A draft with any
-- body, or without theses, stays a draft. posts.status is plain text, so the
-- status itself needs no schema change.
--
-- Status changes made here are not written to post_logs: no one changed the
-- posts, their stage was renamed.
UPDATE posts p
SET status = 'planned'
WHERE p.status = 'draft'
  AND btrim(p.content, E' \t\r\n') = ''
  AND EXISTS (
      SELECT 1 FROM post_notes n
      WHERE n.post_id = p.id AND n.type = 'draft_thesis'
  );
