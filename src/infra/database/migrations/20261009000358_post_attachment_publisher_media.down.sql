ALTER TABLE post_attachments
    DROP COLUMN IF EXISTS publisher_media_uploaded_at,
    DROP COLUMN IF EXISTS publisher_media_url;
