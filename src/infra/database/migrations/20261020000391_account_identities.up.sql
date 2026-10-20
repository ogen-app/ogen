-- External sign-in identities (Google) linked to an account, plus the
-- short-lived rows the OpenID Connect flow needs between redirects.
--
-- An identity is keyed by the provider's stable subject (`sub`), never by email:
-- either side can change its address without breaking the link.

CREATE TABLE account_identities (
    id           TEXT        PRIMARY KEY,
    account_id   TEXT        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    provider     TEXT        NOT NULL CHECK (provider IN ('google')),
    subject      TEXT        NOT NULL,
    -- The provider's address at link or last sign-in, shown in Settings only.
    email        TEXT        NOT NULL,
    linked_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    UNIQUE (provider, subject),
    UNIQUE (account_id, provider)
);

-- An account that signed up with Google has no password until it sets one.
ALTER TABLE accounts ALTER COLUMN password_hash DROP NOT NULL;

-- Serves the case-insensitive email match used to link an existing account.
CREATE INDEX idx_accounts_email_lower ON accounts (lower(email));

-- One row per started sign-in. The raw state lives only in the browser's
-- cookie and the provider redirect; the row is spent once by the callback.
CREATE TABLE oauth_states (
    state_hash    TEXT        PRIMARY KEY,
    provider      TEXT        NOT NULL,
    intent        TEXT        NOT NULL CHECK (intent IN ('signin', 'link', 'invite')),
    nonce         TEXT        NOT NULL,
    pkce_verifier TEXT        NOT NULL,
    return_to     TEXT        NOT NULL DEFAULT '/',
    account_id    TEXT,
    invite_hash   TEXT,
    expires_at    TIMESTAMPTZ NOT NULL,
    consumed_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_oauth_states_expires ON oauth_states (expires_at);

-- A verified provider identity with no account yet, waiting for the user to
-- name their workspace.
CREATE TABLE oauth_pending_signups (
    token_hash  TEXT        PRIMARY KEY,
    provider    TEXT        NOT NULL,
    subject     TEXT        NOT NULL,
    email       TEXT        NOT NULL,
    name        TEXT        NOT NULL DEFAULT '',
    return_to   TEXT        NOT NULL DEFAULT '/',
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_oauth_pending_signups_expires ON oauth_pending_signups (expires_at);

-- How the login behind a new-device alert was made, so the email can say so
-- and securing the account can also detach a Google sign-in.
ALTER TABLE login_alert_tokens
    ADD COLUMN method TEXT NOT NULL DEFAULT 'password' CHECK (method IN ('password', 'google'));
