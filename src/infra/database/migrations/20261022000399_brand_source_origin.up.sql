-- A brand source is an asset brought in to read a brand from: a file uploaded
-- from the Brand offer, or a page its website read imported (CON-399).
-- Automatic workspace analysis skips these.
ALTER TABLE assets DROP CONSTRAINT assets_origin_check;
ALTER TABLE assets
    ADD CONSTRAINT assets_origin_check CHECK (origin IN ('', 'figma', 'brand_distill'));
