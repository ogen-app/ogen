-- Per-membership opt-out from the weekly report email: users holds one row per
-- workspace membership, so an owner of several workspaces turns each report
-- off on its own.
ALTER TABLE users ADD COLUMN weekly_report_opt_out BOOLEAN NOT NULL DEFAULT false;
