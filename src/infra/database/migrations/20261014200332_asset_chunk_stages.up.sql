-- Chunks of an ingest run that is still embedding. An ingest river-service
-- runs step by step stages its parsed chunks here, fills in their vectors a
-- batch at a time, then swaps the embedded ones into assets_chunks in one
-- transaction, so a long document never rides in the job's state.
CREATE TABLE IF NOT EXISTS asset_chunk_stages (
    run_id        text NOT NULL,
    chunk_index   integer NOT NULL,
    tenant_id     text NOT NULL,
    asset_id      text NOT NULL,
    content       text NOT NULL,
    token_count   integer NOT NULL,
    page_start    integer,
    page_end      integer,
    source_label  text,
    source_anchor jsonb,
    embedding     halfvec(3072),
    model         text,
    -- The text's embed failed on its own: it stays out of the chunks.
    failed        boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, chunk_index)
);

-- A new run of an asset drops the asset's earlier runs; stale runs of
-- crashed jobs age out.
CREATE INDEX IF NOT EXISTS asset_chunk_stages_asset_idx ON asset_chunk_stages (asset_id);
CREATE INDEX IF NOT EXISTS asset_chunk_stages_created_idx ON asset_chunk_stages (created_at);
