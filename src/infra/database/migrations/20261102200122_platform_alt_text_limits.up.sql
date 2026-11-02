-- Per-platform alt-text cap on image attachments (image_constraints.max_alt_text_chars).
-- 0 or absent means the platform takes no alt text and the publisher doesn't send it.
--
-- Instagram 1000: docs.zernio.com/platforms/instagram ("longer text is cut at
-- 1,000 characters"). X 1000: the X media-metadata alt_text limit. Threads,
-- Facebook and LinkedIn 1000 and Pinterest 500 are conservative floors pending a
-- check against each platform's API; raising one is a Harbor edit. Alt text was
-- verified end-to-end on Threads (2026-11-02). TikTok, Reddit and YouTube stay at 0:
-- their publishing APIs have no alt-text field.
--
-- Each value is clamped to the global ceiling, which Harbor enforces on every
-- later platform save.
UPDATE platforms SET image_constraints = jsonb_set(image_constraints, '{max_alt_text_chars}',
    to_jsonb(LEAST(1000, (SELECT max_alt_text_chars FROM platform_global_limits WHERE id = 'global'))))
    WHERE id IN ('81mUCmc2xsKd', 'AXqWG7U2qnpt', 'zBU1zqVICGfk', 'rzgpTkARLH0L', 'pQ4yxT3SuE57');

UPDATE platforms SET image_constraints = jsonb_set(image_constraints, '{max_alt_text_chars}',
    to_jsonb(LEAST(500, (SELECT max_alt_text_chars FROM platform_global_limits WHERE id = 'global'))))
    WHERE id = 'Pn4vK8mWz1Bc';
