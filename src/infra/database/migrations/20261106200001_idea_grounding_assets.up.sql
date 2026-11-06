-- The content plan now writes ideas, not planned posts, so an idea carries
-- what such a post carried: whether it came from the campaign's sources
-- (grounding_mode, as on posts) and every asset it drew on (asset_ids; an
-- analysis idea's single source stays in source_asset_id). Both are set at
-- creation and read when the idea becomes a post.
ALTER TABLE ideas
    ADD COLUMN grounding_mode TEXT CHECK (grounding_mode IN ('grounded', 'exploratory')),
    ADD COLUMN asset_ids      TEXT[] NOT NULL DEFAULT '{}';
