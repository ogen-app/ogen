-- source_locator is where in its source asset an analysis idea's first cited
-- passage is: {chunk_id, label, page?, page_end?, slide?, start_ms?, end_ms?}.
-- Older ideas keep null; their labels in sources are all they have.
ALTER TABLE ideas
    ADD COLUMN source_locator JSONB;
