-- The Zernio public URL an attachment was last uploaded to, and when.
--
-- Zernio keeps an upload in temporary storage for 7 days, and any post that
-- references the public URL within that window publishes it. Recording it
-- lets a retried submit reuse finished uploads instead of downloading every
-- attachment from storage and uploading it to Zernio again.
ALTER TABLE post_attachments
    ADD COLUMN publisher_media_url         TEXT NOT NULL DEFAULT '',
    ADD COLUMN publisher_media_uploaded_at TIMESTAMPTZ;
