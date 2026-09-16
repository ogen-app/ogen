-- CON-281: image ingestion. image-service is the single stateless image
-- authority (validate / normalize / EXIF-strip / classify / extract / describe /
-- alt-text); ogen owns persistence + the River `image` queue. These two tables
-- hold content-bank vision state — the searchable output (description + extracted
-- text) lands in the shared assets_chunks with source_anchor {kind:"image"}
-- (CON-280's columns). Both cascade-delete with the asset. Post-attachment images
-- use the light PrepareAttachment path and are NOT recorded here.

-- One vision run per asset. UNIQUE (asset_id, run_key) makes an enqueue
-- idempotent; a re-extract mints a new run_key (additive — prior runs kept).
-- cost_micros is snapshotted at write time from the versioned gemini price table
-- (CON-86) and never recomputed.
CREATE TABLE image_extractions (
    id                   TEXT             PRIMARY KEY,
    tenant_id            TEXT             NOT NULL REFERENCES tenants (id),
    asset_id             TEXT             NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
    run_key              TEXT             NOT NULL,
    status               TEXT             NOT NULL DEFAULT 'pending'
                                          CHECK (status IN ('pending', 'normalizing', 'classifying',
                                                            'extracting', 'describing', 'partial',
                                                            'complete', 'failed')),
    shape                TEXT             NOT NULL DEFAULT '',
    classify_confidence  DOUBLE PRECISION NOT NULL DEFAULT 0,
    classify_model       TEXT             NOT NULL DEFAULT '',
    extract_model        TEXT             NOT NULL DEFAULT '',
    escalate_model       TEXT             NOT NULL DEFAULT '',
    escalated            BOOLEAN          NOT NULL DEFAULT false,
    escalation_improved  BOOLEAN          NOT NULL DEFAULT false,
    description_ok       BOOLEAN          NOT NULL DEFAULT false,
    extraction_ok        BOOLEAN          NOT NULL DEFAULT false,
    truncated            BOOLEAN          NOT NULL DEFAULT false,
    normalized_s3_key    TEXT,
    normalized_mime      TEXT             NOT NULL DEFAULT '',
    width                INT              NOT NULL DEFAULT 0,
    height               INT              NOT NULL DEFAULT 0,
    is_animated          BOOLEAN          NOT NULL DEFAULT false,
    checksum_sha256      TEXT             NOT NULL DEFAULT '',
    input_tokens         BIGINT           NOT NULL DEFAULT 0,
    output_tokens        BIGINT           NOT NULL DEFAULT 0,
    cost_micros          BIGINT           NOT NULL DEFAULT 0,
    price_version        TEXT             NOT NULL DEFAULT '',
    failure_reason       TEXT             NOT NULL DEFAULT '',
    -- Stable, machine-readable companion to failure_reason (CON-281): the client
    -- matches the code (quota / unsupported / unavailable / partial) and falls
    -- back to the prose when it is unknown. Empty on a clean complete run.
    failure_code         TEXT             NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ      NOT NULL DEFAULT now()
);

-- Idempotency key + the "latest run for this asset" read.
CREATE UNIQUE INDEX idx_image_extractions_asset_runkey ON image_extractions (asset_id, run_key);
CREATE INDEX idx_image_extractions_asset ON image_extractions (asset_id, created_at DESC);

-- One structured region per extraction, mirroring the documents-service Block
-- shape. cells/anchor are jsonb (table grid / normalized image-region bbox).
CREATE TABLE image_blocks (
    id             TEXT        PRIMARY KEY,
    tenant_id      TEXT        NOT NULL REFERENCES tenants (id),
    extraction_id  TEXT        NOT NULL REFERENCES image_extractions (id) ON DELETE CASCADE,
    asset_id       TEXT        NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
    index          INT         NOT NULL,
    kind           TEXT        NOT NULL,
    level          INT         NOT NULL DEFAULT 0,
    text           TEXT        NOT NULL DEFAULT '',
    cells          JSONB,
    anchor         JSONB,
    provenance     TEXT        NOT NULL DEFAULT '',
    low_confidence BOOLEAN     NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_image_blocks_extraction_index ON image_blocks (extraction_id, index);
CREATE INDEX idx_image_blocks_asset ON image_blocks (asset_id);

-- CON-281 D5: guard user-edited alt text from being overwritten by
-- (re-)generation, on BOTH the content-bank asset and the post-attachment.
ALTER TABLE assets           ADD COLUMN alt_text_edited_by_user BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE post_attachments ADD COLUMN alt_text_edited_by_user BOOLEAN NOT NULL DEFAULT false;
