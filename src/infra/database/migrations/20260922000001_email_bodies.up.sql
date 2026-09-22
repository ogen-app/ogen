-- CON-306: persist the rendered body at send so the operator Emails tab (CON-192)
-- renders it even after the Resend message ages out of retention (or the Resend
-- key is unset). This reverses CON-298's live-fetch-only decision; the live
-- Resend fetch remains a fallback for rows sent before this shipped.
--
-- Side table, 1:1 with email_logs, so the large html/text stay out of the hot
-- ListTenantEmails query. ON DELETE CASCADE ties the body to its parent log so
-- the existing retention sweep (DELETE FROM email_logs WHERE created_at < cutoff)
-- reaps it — no separate cleanup, and the body ages out on OUR schedule
-- (EMAIL_LOG_RETENTION_DAYS), decoupled from Resend's retention window.
--
-- cc/bcc are intentionally omitted: the send path only ever sets a single To
-- (email.Message), so they'd always be empty here; the live-fetch fallback still
-- fills them when Resend has them.
CREATE TABLE email_bodies (
    email_log_id TEXT        PRIMARY KEY REFERENCES email_logs (id) ON DELETE CASCADE,
    subject      TEXT        NOT NULL DEFAULT '',
    html         TEXT        NOT NULL DEFAULT '',
    text         TEXT        NOT NULL DEFAULT '',
    from_addr    TEXT        NOT NULL DEFAULT '',
    reply_to     TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
