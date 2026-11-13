-- An offer can follow another: made afresh because what a confirmed offer
-- was made on changed while it waited (replaces), or trying again the items
-- a confirmed offer's run left undone (retry_of). Both name an offer in the
-- same thread, or are ''.
ALTER TABLE assistant_proposals
    ADD COLUMN replaces TEXT NOT NULL DEFAULT '',
    ADD COLUMN retry_of TEXT NOT NULL DEFAULT '';

-- A pending offer that was offered afresh is no longer the one to answer.
CREATE INDEX idx_assistant_proposals_replaces
    ON assistant_proposals (replaces)
    WHERE replaces <> '' AND status = 'pending';
