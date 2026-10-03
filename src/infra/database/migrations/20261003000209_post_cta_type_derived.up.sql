-- cta_type is derived from cta_url on every write ('link' when a URL is set,
-- 'none' otherwise). Bring stored rows in line, retiring the unused 'button'.
UPDATE posts
SET cta_type = CASE WHEN btrim(cta_url) <> '' THEN 'link' ELSE 'none' END
WHERE cta_type IS DISTINCT FROM CASE WHEN btrim(cta_url) <> '' THEN 'link' ELSE 'none' END;
