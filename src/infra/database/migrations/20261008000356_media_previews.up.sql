-- CON-356: Figma-ready preview copies of post media.
--
-- figma.createImage takes PNG, JPEG or GIF up to 4096 px on the long edge. An
-- image or video poster that doesn't qualify is rendered once by image-service
-- and the copy is recorded here, so the next campaign read reuses it.
--
-- source_ref names what was rendered: the attachment's checksum_sha256 for an
-- image (identical uploads share one copy), the poster's storage key for a
-- video. Rows cascade from tenants; the stored copies live under the tenant's
-- key prefix.
CREATE TABLE media_previews (
    tenant_id     TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    source        TEXT        NOT NULL CHECK (source IN ('image', 'poster')),
    source_ref    TEXT        NOT NULL,
    max_long_edge INTEGER     NOT NULL,
    s3_key        TEXT        NOT NULL,
    mime_type     TEXT        NOT NULL,
    width         INTEGER     NOT NULL,
    height        INTEGER     NOT NULL,
    size_bytes    BIGINT      NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, source, source_ref, max_long_edge)
);
