-- The ideas a post was made from. A post can be compiled from several ideas,
-- and one idea can feed several posts (platform versions of one insight), so
-- the link is many-to-many. It is the provenance the learning loop reads:
-- which ideas became which posts, and how those performed.
--
-- position orders a post's ideas as they were compiled. Deleting either side
-- drops the link.
CREATE TABLE post_ideas (
    post_id    TEXT        NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
    idea_id    TEXT        NOT NULL REFERENCES ideas (id) ON DELETE CASCADE,
    tenant_id  TEXT        NOT NULL REFERENCES tenants (id),
    position   INT         NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (post_id, idea_id)
);

CREATE INDEX post_ideas_idea_idx ON post_ideas (tenant_id, idea_id);
