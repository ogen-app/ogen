-- What a confirmed offer's run did: the answer's sentence, how much of the
-- job it made and the work's result. A reloaded thread settles the offer's
-- card with it. NULL until the offer ran.
ALTER TABLE assistant_proposals
    ADD COLUMN outcome JSONB;
