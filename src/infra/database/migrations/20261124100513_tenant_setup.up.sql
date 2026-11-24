-- Setup after signup, per workspace. A NULL setup_status is a workspace made
-- before setup existed: it is never sent through it. Step, reached and role
-- are the client's progress; finished_* say who ended setup and when.
ALTER TABLE tenants
    ADD COLUMN setup_status      TEXT CHECK (setup_status IN ('pending', 'done', 'skipped')),
    ADD COLUMN setup_step        TEXT NOT NULL DEFAULT '',
    ADD COLUMN setup_reached     TEXT NOT NULL DEFAULT '',
    ADD COLUMN setup_role        TEXT NOT NULL DEFAULT '' CHECK (setup_role IN ('', 'own', 'client')),
    ADD COLUMN setup_run_id      TEXT,
    ADD COLUMN setup_updated_at  TIMESTAMPTZ,
    ADD COLUMN setup_finished_by TEXT,
    ADD COLUMN setup_finished_at TIMESTAMPTZ;

-- Finds the workspaces that skipped setup or never finished it.
CREATE INDEX tenants_setup_status_idx ON tenants (setup_status) WHERE setup_status IS NOT NULL;
