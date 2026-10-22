UPDATE assets SET origin = '', origin_ref = NULL WHERE origin = 'brand_distill';
ALTER TABLE assets DROP CONSTRAINT assets_origin_check;
ALTER TABLE assets ADD CONSTRAINT assets_origin_check CHECK (origin IN ('', 'figma'));
