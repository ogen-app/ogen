-- A post's rhetorical shape (how-to, explainer, ...). Nullable: no format is
-- the normal case. The vocabulary is validated by the API, not the schema.
ALTER TABLE posts ADD COLUMN content_format TEXT;
