-- merged_at stamps an idea other ideas were merged into. A merge is a
-- decision about the kept idea, so the clean-ups that delete an asset's
-- untriaged analysis ideas (re-analysis, lens removal, asset delete) leave
-- it, and with it what the merged ideas brought.
ALTER TABLE ideas
    ADD COLUMN merged_at TIMESTAMPTZ;
