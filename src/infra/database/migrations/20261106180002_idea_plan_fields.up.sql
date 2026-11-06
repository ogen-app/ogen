-- An idea can carry a plan: where it would land if it became a post. Planning
-- (the content plan, the assistant) writes these as suggestions; turning the
-- idea into a post reads them. They are not references the database keeps
-- true: a platform switched off or a phase renamed leaves the suggestion to
-- be re-checked when the post is made.
ALTER TABLE ideas
    ADD COLUMN plan_platform_id TEXT,
    ADD COLUMN plan_post_type   TEXT,
    ADD COLUMN plan_phase_id    TEXT,
    ADD COLUMN plan_target_date DATE;

-- Ideas now also come from the content plan and from the assistant.
ALTER TABLE ideas DROP CONSTRAINT ideas_origin_check;
ALTER TABLE ideas
    ADD CONSTRAINT ideas_origin_check CHECK (origin IN ('', 'asset_analysis', 'content_plan', 'assistant'));

-- Dated ideas are read by date for the calendar.
CREATE INDEX ideas_plan_target_date_idx ON ideas (tenant_id, plan_target_date) WHERE plan_target_date IS NOT NULL;
