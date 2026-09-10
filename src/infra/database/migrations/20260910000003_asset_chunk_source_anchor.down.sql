-- CON-280 rollback: drop the source-anchoring columns.
ALTER TABLE assets_chunks
    DROP COLUMN source_anchor;

ALTER TABLE assets_chunks
    DROP COLUMN source_label;
