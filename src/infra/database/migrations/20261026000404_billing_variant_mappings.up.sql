-- Which Lemon Squeezy variant sells each tier version price row, set from
-- Harbor instead of the LEMONSQUEEZY_VARIANTS env. Test and live are mapped
-- side by side (variant ids differ per mode), so the live map can be prepared
-- before the switch to live payments.
--
-- One variant sells at most one price per mode: webhooks resolve the price
-- from the variant. Deleting a draft's price rows drops their mappings.

CREATE TABLE billing_variant_mappings (
    tier_version_price_id TEXT        NOT NULL REFERENCES tenant_tier_version_prices (id) ON DELETE CASCADE,
    test_mode             BOOLEAN     NOT NULL,
    variant_id            TEXT        NOT NULL CHECK (variant_id <> ''),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- harbor, or env-seed for the one-off import of LEMONSQUEEZY_VARIANTS.
    updated_by            TEXT        NOT NULL,
    reason                TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (tier_version_price_id, test_mode),
    CONSTRAINT uq_billing_variant_mappings_variant UNIQUE (variant_id, test_mode)
);
