-- Billing with Lemon Squeezy as merchant of record.
--
-- Ogen mirrors each provider subscription, logs every webhook delivery, and
-- schedules downgrades for the billing boundary. The tables are provider
-- neutral (provider + provider_* columns); status keeps the provider's own
-- vocabulary because the UI contract passes it through.
--
-- The tables carry a plain tenant_id rather than the TenantScoped model hook:
-- webhook ingest and the reconcile sweep resolve the tenant FROM the row, so
-- every repository method takes the tenant explicitly.

CREATE TABLE billing_subscriptions (
    id                       TEXT        PRIMARY KEY,
    tenant_id                TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    -- The account that started the checkout. It drives the once-per-account
    -- free month, so it outlives a removed membership but not the account.
    account_id               TEXT        REFERENCES accounts (id) ON DELETE SET NULL,
    provider                 TEXT        NOT NULL CHECK (provider IN ('lemonsqueezy')),
    provider_subscription_id TEXT        NOT NULL,
    provider_customer_id     TEXT        NOT NULL DEFAULT '',
    provider_order_id        TEXT        NOT NULL DEFAULT '',
    provider_variant_id      TEXT        NOT NULL DEFAULT '',
    -- NULL when the variant is not in the configured variant map.
    tier_version_price_id    TEXT        REFERENCES tenant_tier_version_prices (id) ON DELETE SET NULL,
    -- No CHECK: the vocabulary is the provider's; unknown values are kept as-is.
    status                   TEXT        NOT NULL,
    test_mode                BOOLEAN     NOT NULL,
    renews_at                TIMESTAMPTZ,
    ends_at                  TIMESTAMPTZ,
    trial_ends_at            TIMESTAMPTZ,
    -- Set once by the first successful non-zero payment; NULL = never paid.
    first_paid_at            TIMESTAMPTZ,
    card_brand               TEXT        NOT NULL DEFAULT '',
    card_last_four           TEXT        NOT NULL DEFAULT '',
    -- The provider's own updated_at: the last-write-wins staleness guard.
    provider_updated_at      TIMESTAMPTZ NOT NULL,
    invalidated_at           TIMESTAMPTZ,
    invalidated_reason       TEXT        NOT NULL DEFAULT '',
    -- When the never-paid trial teardown was enqueued (idempotency).
    teardown_at              TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_subscription_id)
);
CREATE INDEX idx_billing_subscriptions_tenant ON billing_subscriptions (tenant_id) WHERE invalidated_at IS NULL;
CREATE INDEX idx_billing_subscriptions_trial ON billing_subscriptions (account_id, test_mode) WHERE trial_ends_at IS NOT NULL;

-- Raw ingest log of provider webhooks. Lemon Squeezy sends no event id, so a
-- redelivery is recognised by the hash of its body. Pruned after 90 days.
CREATE TABLE billing_webhook_events (
    id                 TEXT        PRIMARY KEY,
    provider           TEXT        NOT NULL,
    event_name         TEXT        NOT NULL,
    body_sha256        TEXT        NOT NULL UNIQUE,
    provider_object_id TEXT        NOT NULL DEFAULT '',
    tenant_id          TEXT        REFERENCES tenants (id) ON DELETE SET NULL,
    test_mode          BOOLEAN     NOT NULL DEFAULT false,
    received_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at       TIMESTAMPTZ,
    outcome            TEXT        NOT NULL DEFAULT ''
                       CHECK (outcome IN ('', 'applied', 'stale', 'ignored_mode', 'ignored_unknown', 'failed')),
    error              TEXT        NOT NULL DEFAULT '',
    payload            JSONB       NOT NULL
);
CREATE INDEX idx_billing_webhook_events_tenant ON billing_webhook_events (tenant_id, received_at DESC);
CREATE INDEX idx_billing_webhook_events_received ON billing_webhook_events (received_at);
CREATE INDEX idx_billing_webhook_events_failed ON billing_webhook_events (received_at) WHERE outcome = 'failed';

-- A downgrade that lands at the billing boundary. At effective_at - 1h the
-- provider is switched to the cheaper variant (provider_applied); at
-- effective_at the tier assignment moves (applied).
CREATE TABLE billing_scheduled_changes (
    id                         TEXT        PRIMARY KEY,
    tenant_id                  TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    billing_subscription_id    TEXT        NOT NULL REFERENCES billing_subscriptions (id) ON DELETE CASCADE,
    from_tier_version_price_id TEXT        NOT NULL REFERENCES tenant_tier_version_prices (id) ON DELETE RESTRICT,
    to_tier_version_price_id   TEXT        NOT NULL REFERENCES tenant_tier_version_prices (id) ON DELETE RESTRICT,
    effective_at               TIMESTAMPTZ NOT NULL,
    status                     TEXT        NOT NULL DEFAULT 'pending'
                               CHECK (status IN ('pending', 'provider_applied', 'applied', 'cancelled', 'failed')),
    created_by                 TEXT        NOT NULL DEFAULT '',
    error                      TEXT        NOT NULL DEFAULT '',
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- At most one open change per subscription.
CREATE UNIQUE INDEX uq_billing_scheduled_changes_open ON billing_scheduled_changes (billing_subscription_id)
    WHERE status IN ('pending', 'provider_applied');
CREATE INDEX idx_billing_scheduled_changes_due ON billing_scheduled_changes (effective_at)
    WHERE status IN ('pending', 'provider_applied');

-- New workspaces are gated until they check out. Existing rows stay false, so
-- every workspace that predates billing is grandfathered.
ALTER TABLE tenants ADD COLUMN billing_required BOOLEAN NOT NULL DEFAULT false;

-- The one tier carrying the marketing free month. The mark lives on the tier,
-- not on its immutable versions, so publishing a new version keeps it.
ALTER TABLE tenant_tiers ADD COLUMN free_trial BOOLEAN NOT NULL DEFAULT false;
CREATE UNIQUE INDEX uq_tenant_tiers_free_trial ON tenant_tiers (free_trial) WHERE free_trial;

-- A first subscription moves the workspace onto the purchased version with
-- reason 'subscription_started'. The new set is a strict superset of the old
-- one, so the constraint is added NOT VALID (nothing to validate, no scan).
ALTER TABLE tenant_tier_assignments DROP CONSTRAINT tenant_tier_assignments_reason_check;
ALTER TABLE tenant_tier_assignments ADD CONSTRAINT tenant_tier_assignments_reason_check
    CHECK (reason IN ('signup', 'migration_accepted', 'migration_lapsed', 'grandfathered', 'upgrade', 'downgrade', 'operator_set', 'subscription_started')) NOT VALID;
