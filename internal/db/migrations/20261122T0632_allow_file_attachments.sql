-- Allow arbitrary file attachments (not only PNG/JPEG/GIF) in attachments.
ALTER TABLE attachments DROP CONSTRAINT IF EXISTS attachments_media_type_check;
ALTER TABLE attachments ADD CONSTRAINT attachments_media_type_check
    CHECK (btrim(media_type) <> '' AND length(media_type) <= 255);
