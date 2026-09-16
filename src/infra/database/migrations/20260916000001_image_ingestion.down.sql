-- CON-281 rollback.
ALTER TABLE post_attachments DROP COLUMN IF EXISTS alt_text_edited_by_user;
ALTER TABLE assets           DROP COLUMN IF EXISTS alt_text_edited_by_user;

DROP TABLE IF EXISTS image_blocks;
DROP TABLE IF EXISTS image_extractions;
