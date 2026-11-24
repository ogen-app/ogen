-- What a site's homepage says about the site: its name, and its icon copied
-- into our storage. Set on URL assets of a site root when their page states
-- them; NULL otherwise.
ALTER TABLE assets ADD COLUMN site_meta JSONB NULL;
