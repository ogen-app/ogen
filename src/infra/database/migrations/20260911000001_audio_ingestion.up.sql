-- CON-282: audio ingestion. audio-service is stateless compute (probe /
-- normalize / transcribe-segment); ogen owns the resumable state machine on a
-- dedicated River `audio` queue. These three tables hold ONLY processing
-- state — the searchable output (assembled, embedded, time-anchored transcript
-- chunks) lands in the shared assets_chunks with source_anchor {kind:"time"}
-- (CON-280's columns). All three cascade-delete with the asset (D5: the asset's
-- audio_* rows and its normalized.opus derivative are evicted together).

-- One transcription run per asset. UNIQUE (asset_id, run_key) makes an enqueue
-- idempotent; a re-extract mints a new run_key. cost_micros is snapshotted at
-- write time from the versioned gemini price table (CON-86) — never recomputed.
CREATE TABLE audio_extractions (
    id                  TEXT        PRIMARY KEY,
    tenant_id           TEXT        NOT NULL REFERENCES tenants (id),
    asset_id            TEXT        NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
    run_key             TEXT        NOT NULL,
    status              TEXT        NOT NULL DEFAULT 'pending'
                                    CHECK (status IN ('pending', 'normalizing', 'transcribing',
                                                      'partial', 'complete', 'failed')),
    detected_language   TEXT        NOT NULL DEFAULT '',
    source_duration_ms  BIGINT      NOT NULL DEFAULT 0,
    normalized_s3_key   TEXT,
    segment_count       INT         NOT NULL DEFAULT 0,
    transcribe_model    TEXT        NOT NULL DEFAULT '',
    embed_model         TEXT        NOT NULL DEFAULT '',
    audio_seconds       BIGINT      NOT NULL DEFAULT 0,
    cost_micros         BIGINT      NOT NULL DEFAULT 0,
    price_version       TEXT        NOT NULL DEFAULT '',
    failure_reason      TEXT        NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Idempotency key + the "latest run for this asset" read.
CREATE UNIQUE INDEX idx_audio_extractions_asset_runkey ON audio_extractions (asset_id, run_key);
CREATE INDEX idx_audio_extractions_asset ON audio_extractions (asset_id, created_at DESC);

-- One bounded transcription window per extraction, checkpointed so a retry
-- resumes from the first incomplete segment. [start_ms, end_ms) are on the
-- ORIGINAL asset timeline.
CREATE TABLE audio_segments (
    id              TEXT        PRIMARY KEY,
    tenant_id       TEXT        NOT NULL REFERENCES tenants (id),
    extraction_id   TEXT        NOT NULL REFERENCES audio_extractions (id) ON DELETE CASCADE,
    asset_id        TEXT        NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
    index           INT         NOT NULL,
    start_ms        BIGINT      NOT NULL,
    end_ms          BIGINT      NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending', 'done', 'failed')),
    retry_count     INT         NOT NULL DEFAULT 0,
    failure_reason  TEXT        NOT NULL DEFAULT '',
    utterance_count INT         NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_audio_segments_extraction_index ON audio_segments (extraction_id, index);
-- The resume scan: first incomplete segment of an extraction, in order.
CREATE INDEX idx_audio_segments_extraction_status ON audio_segments (extraction_id, index)
    WHERE status <> 'done';

-- Raw transcript spans — the source for chunk assembly and the transcript API.
-- [start_ms, end_ms) are on the ORIGINAL asset timeline.
CREATE TABLE utterances (
    id          TEXT        PRIMARY KEY,
    tenant_id   TEXT        NOT NULL REFERENCES tenants (id),
    segment_id  TEXT        NOT NULL REFERENCES audio_segments (id) ON DELETE CASCADE,
    asset_id    TEXT        NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
    index       INT         NOT NULL,
    start_ms    BIGINT      NOT NULL,
    end_ms      BIGINT      NOT NULL,
    text        TEXT        NOT NULL,
    confidence  DOUBLE PRECISION NOT NULL DEFAULT -1,
    language    TEXT        NOT NULL DEFAULT '',
    is_speech   BOOLEAN     NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Transcript read: all utterances of an asset in timeline order.
CREATE INDEX idx_utterances_asset_start ON utterances (asset_id, start_ms);
CREATE INDEX idx_utterances_segment ON utterances (segment_id, index);

-- Widen the asset-type check to admit AUDIO (CON-282). This also (re)adds 'DOC':
-- CON-280 introduced the DOC asset type but never widened this constraint (the
-- last widening was 'IMG' in CON-246), so document uploads would fail the check
-- too — backfilled here alongside AUDIO.
ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_type_check;
ALTER TABLE assets
    ADD CONSTRAINT assets_type_check CHECK (type IS NULL OR type IN ('MD', 'PDF', 'URL', 'IMG', 'DOC', 'AUDIO'));
