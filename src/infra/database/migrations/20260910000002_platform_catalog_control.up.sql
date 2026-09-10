-- CON-292: operator-controlled platform catalog.
--
-- Make the `platforms` row self-sufficient so the hardcoded Go registry
-- (supportedPlatforms + sqidToZernioID in src/infra/publishers/zernio/platforms.go)
-- can be deleted and the row becomes the single source of truth. Adds the
-- catalog fields the registry used to carry, backfills them for the 6 seeded
-- platforms from the exact registry values (so day-one behaviour is identical),
-- and introduces platform_global_limits for the cross-platform safety ceilings
-- that were Go constants. All additive; existing constraint jsonb is untouched.

-- 1. New catalog columns (additive; safe defaults keep existing rows valid).
ALTER TABLE platforms ADD COLUMN zernio_id            TEXT    NOT NULL DEFAULT '';
ALTER TABLE platforms ADD COLUMN enabled              BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE platforms ADD COLUMN connect_supported    BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE platforms ADD COLUMN supported_post_types JSONB   NOT NULL DEFAULT '[]';
ALTER TABLE platforms ADD COLUMN sort_order           INT     NOT NULL DEFAULT 0;

-- 2. Backfill the 6 seeded platforms from the retiring Go registry, verbatim.
--    zernio_id replaces sqidToZernioID; supported_post_types replaces
--    SupportedPlatform.SupportedPostTypes; sort_order follows the registry's
--    display order so GET /api/platforms renders in the same order as today.
UPDATE platforms SET zernio_id = 'twitter',   sort_order = 10,
    supported_post_types = '["text-post","image-post","video","thread"]'
    WHERE id = '81mUCmc2xsKd';
UPDATE platforms SET zernio_id = 'linkedin',  sort_order = 20,
    supported_post_types = '["text-post","image-post","carousel","video","article"]'
    WHERE id = 'AXqWG7U2qnpt';
UPDATE platforms SET zernio_id = 'facebook',  sort_order = 30,
    supported_post_types = '["text-post","image-post","video","reel","link-post"]'
    WHERE id = 'zBU1zqVICGfk';
UPDATE platforms SET zernio_id = 'instagram', sort_order = 40,
    supported_post_types = '["image-post","carousel","reel","story"]'
    WHERE id = 'rzgpTkARLH0L';
UPDATE platforms SET zernio_id = 'youtube',   sort_order = 50,
    supported_post_types = '["video","short"]'
    WHERE id = '8S8bWQTG6qD';
UPDATE platforms SET zernio_id = 'threads',   sort_order = 60,
    supported_post_types = '["text-post","image-post","carousel","video","thread"]'
    WHERE id = 'pQ4yxT3SuE57';

-- 3. One row per Zernio slug. Partial so freshly-created rows (zernio_id = '')
--    don't collide before an operator assigns a slug.
CREATE UNIQUE INDEX ux_platforms_zernio_id ON platforms (zernio_id) WHERE zernio_id <> '';

-- 4. Global safety ceilings — single-row config replacing the Go constants
--    (maxImageUploadBytes / maxPDFUploadBytes / maxVideoUploadBytes /
--    maxAltTextLen / MaxThreadSegments). Seeded with the current values
--    verbatim so the enforced caps are byte-for-byte identical on day one.
CREATE TABLE platform_global_limits (
    id                     TEXT        PRIMARY KEY DEFAULT 'global',
    max_image_upload_bytes BIGINT      NOT NULL,
    max_pdf_upload_bytes   BIGINT      NOT NULL,
    max_video_upload_bytes BIGINT      NOT NULL,
    max_alt_text_chars     INT         NOT NULL,
    max_thread_segments    INT         NOT NULL,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT single_row CHECK (id = 'global')
);

INSERT INTO platform_global_limits
    (id, max_image_upload_bytes, max_pdf_upload_bytes, max_video_upload_bytes, max_alt_text_chars, max_thread_segments)
VALUES
    ('global', 52428800, 104857600, 5368709120, 2000, 25);

-- 5. Seed the next wave of platforms — TikTok / Pinterest / Reddit — DISABLED
--    (CON-292 §19.2). Day-one tenant behaviour is unchanged (they don't surface
--    until an operator flips enabled=true); turning one on is a one-toggle launch
--    once its account connect is verified. Media/text limits verified against
--    docs.zernio.com (2026-09-10): per-platform pages for tiktok/pinterest/reddit.
--    enabled is set explicitly here because bun would coerce a zero-value false
--    back to the column DEFAULT true on insert — raw SQL is the seam for disabled
--    seeds. All caps are below the global upload ceilings (§12).
INSERT INTO platforms
    (id, name, post_types, cadence, constraints, image_constraints, pdf_constraints, video_constraints, text_constraints, zernio_id, enabled, connect_supported, supported_post_types, sort_order)
VALUES
(
    'Tk7nQ2xLpR9a', 'TikTok',
    '{"video":"Video","image-post":"Photo","carousel":"Photo carousel (up to 35 images)"}',
    '1 video per day',
    'short vertical video (3s–10min); captions up to 2200 chars; photo carousels up to 35 images with a 4000-char description',
    '{"max_file_size_bytes":20971520,"allowed_formats":["jpeg","png","webp"],"animated_gif_supported":false,"max_attachments_per_post":35}',
    '{}',
    '{"max_file_size_bytes":4294967296,"allowed_formats":["mp4","mov","webm"],"max_duration_seconds":600,"min_duration_seconds":3,"max_width":0,"max_height":0,"allowed_aspect_ratios":["9:16","1:1","16:9"],"max_attachments_per_post":1}',
    '{"max_content_chars":2200,"per_post_type":{"carousel":4000}}',
    'tiktok', false, true,
    '["video","image-post","carousel"]', 70
),
(
    'Pn4vK8mWz1Bc', 'Pinterest',
    '{"image-post":"Image Pin","video":"Video Pin"}',
    '3–5 pins per week',
    'single image or video pins (no carousels); titles up to 100 chars; descriptions up to 800 chars; every pin requires a board',
    '{"max_file_size_bytes":33554432,"allowed_formats":["jpeg","png","webp","gif"],"animated_gif_supported":true,"max_attachments_per_post":1}',
    '{}',
    '{"max_file_size_bytes":2147483648,"allowed_formats":["mp4","mov"],"max_duration_seconds":900,"min_duration_seconds":4,"max_width":0,"max_height":0,"allowed_aspect_ratios":["2:3","1:1","9:16"],"max_attachments_per_post":1}',
    '{"max_content_chars":800,"max_title_chars":100}',
    'pinterest', false, true,
    '["image-post","video"]', 80
),
(
    'Rd5hJ3yTq6Ne', 'Reddit',
    '{"text-post":"Text post","link-post":"Link post","image-post":"Image post","carousel":"Gallery (2–20 images)","video":"Video"}',
    '2–3 posts per week',
    'title up to 300 chars (required, not editable); body up to 40000 chars; posts to a chosen subreddit',
    '{"max_file_size_bytes":20971520,"allowed_formats":["jpeg","png","gif"],"animated_gif_supported":true,"max_attachments_per_post":20}',
    '{}',
    '{"max_file_size_bytes":1073741824,"allowed_formats":["mp4"],"max_duration_seconds":0,"min_duration_seconds":0,"max_width":0,"max_height":0,"allowed_aspect_ratios":[],"max_attachments_per_post":1}',
    '{"max_content_chars":40000,"max_title_chars":300}',
    'reddit', false, true,
    '["text-post","link-post","image-post","carousel","video"]', 90
);
