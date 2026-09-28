-- Known devices and new-device login alerts.
--
-- Both tables are account-level (the global login identity), not tenant-scoped:
-- a browser recognised for an account is recognised in every workspace it can
-- open. Only hashes of the device cookie and the alert token are stored.

CREATE TABLE account_known_devices (
    id            TEXT        PRIMARY KEY,
    account_id    TEXT        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    device_hash   TEXT        NOT NULL,
    device_label  TEXT        NOT NULL DEFAULT '',
    user_agent    TEXT        NOT NULL DEFAULT '',
    last_ip       TEXT        NOT NULL DEFAULT '',
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_id, device_hash)
);
-- The retention sweep drops devices unseen for longer than the cookie lives.
CREATE INDEX idx_account_known_devices_last_seen ON account_known_devices (last_seen_at);

-- Set when an account first enrols a device, cleared only by the secure-account
-- action. "First device, no alert" is decided by this marker, not by counting
-- rows: the retention sweep can remove every device of a dormant account, and
-- the next unfamiliar login to it must still be reported.
ALTER TABLE accounts ADD COLUMN devices_enrolled_at TIMESTAMPTZ;

-- An alert token is the capability behind the email's "This wasn't me" link.
-- user_id/tenant_id are the membership the login opened, kept for attribution
-- only: the membership can be gone by the time the link is used, so they carry
-- no foreign key and the secure action re-resolves one from account_id.
CREATE TABLE login_alert_tokens (
    id           TEXT        PRIMARY KEY,
    account_id   TEXT        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    device_id    TEXT        REFERENCES account_known_devices (id) ON DELETE SET NULL,
    user_id      TEXT        NOT NULL,
    tenant_id    TEXT        NOT NULL,
    token_hash   TEXT        NOT NULL UNIQUE,
    ip           TEXT        NOT NULL DEFAULT '',
    device_label TEXT        NOT NULL DEFAULT '',
    location     TEXT        NOT NULL DEFAULT '',
    login_at     TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    consumed_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Serves the per-account hourly alert cap and voiding an account's tokens.
CREATE INDEX idx_login_alert_tokens_account_created ON login_alert_tokens (account_id, created_at);
