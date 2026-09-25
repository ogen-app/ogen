-- CON-312: surface why an ingestion failed. DOC assets had only status=failed;
-- assets now carry a tenant-visible reason + a stable machine-readable code
-- (models.UploadCode*), set only while status = 'failed'. Audio extractions had
-- a reason but no code (image extractions already have both).
ALTER TABLE assets
    ADD COLUMN failure_code   TEXT NOT NULL DEFAULT '',
    ADD COLUMN failure_reason TEXT NOT NULL DEFAULT '';

ALTER TABLE audio_extractions
    ADD COLUMN failure_code TEXT NOT NULL DEFAULT '';
