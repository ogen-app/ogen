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
