-- CON-280: document-service source anchoring. Office/text documents
-- (.docx/.pptx/.xlsx/.odt/... ) are chunked with an exact source location so a
-- retrieved chunk can be cited as "Slide 4", "Sheet 'Q3 Pipeline' rows 10-24",
-- or "Onboarding Guide > Authentication > SSO" instead of an opaque page number.
--
-- Two additive, nullable columns carry that citation. Both stay NULL for every
-- existing row and for PDF/URL/MD chunks (today's behaviour); page_start/page_end
-- are retained and keep populating for page-flow chunks. CON-281 (image-service)
-- and CON-282 (audio-service) reuse these same two columns for their own anchor
-- kinds (block/time), so this migration is their shared foundation.

-- source_label is the human-readable citation shown alongside a retrieved chunk.
ALTER TABLE assets_chunks
    ADD COLUMN source_label text;

-- source_anchor is the structured, machine-usable location:
--   {"kind":"slide","slide":4}
--   {"kind":"sheet","sheet":"Q3 Pipeline","cell_range":"A10:F24"}
--   {"kind":"section","heading_path":["Onboarding Guide","Authentication","SSO"]}
--   {"kind":"page","page":7}
-- jsonb (not typed columns) because the shape varies by anchor kind and is only
-- ever read back whole, never filtered on in SQL.
ALTER TABLE assets_chunks
    ADD COLUMN source_anchor jsonb;
