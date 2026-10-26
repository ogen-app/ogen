# Billing: sandbox setup and the switch to live payments

Lemon Squeezy (LS) is Ogen's merchant of record. Billing runs in LS **test mode**
until the store is activated, then switches to **live** in one maintenance window.
Test and live have separate API keys, separate products and variants (different
ids) and separate webhooks, so the switch touches the secret store, the env, the
variant map and every subscription made in test mode.

Settings involved:

| Where | Name | Test period | Live |
| -- | -- | -- | -- |
| env | `BILLING_PROVIDER` | `lemonsqueezy` | `lemonsqueezy` |
| env | `LEMONSQUEEZY_TEST_MODE` | `true` | `false` |
| env | `LEMONSQUEEZY_STORE_ID` | store id | same store id |
| env | `BILLING_TRIAL_TEARDOWN_LIVE` | `false`, then `true` once verified | `true` |
| Harbor → Secrets | `lemonsqueezy_api_key` | test key | live key |
| Harbor → Secrets | `lemonsqueezy_webhook_secret` | test signing secret | live signing secret |
| Harbor → Secrets | `lemonsqueezy_api_key_live` | live key (optional, to prepare the live map) | not needed |
| Harbor → Billing → Variant mapping | test map | test variant ids | (kept, unused) |
| Harbor → Billing → Variant mapping | live map | prepared ahead | live variant ids |

## The variant map

Each sellable price row (a non-zero price of a published, purchasable tier
version) is sold through one LS variant. The map is kept in the database, per
mode, and edited in **Harbor → Billing → Variant mapping** (`BillingAdminService`
`ListVariantMappings` / `SetVariantMapping` / `SuggestVariantMappings`). LS's API
cannot create products or variants, so those are made in the LS dashboard; the
rest is checked by Ogen:

- the variant must exist in that mode's store, be published and bill **every
  month or every year** like the price row;
- its price must equal the row's `net_minor`, **excluding tax**, in the store's
  currency;
- on the free-month tier, it must carry a **1-month free trial**;
- one variant sells one price per mode.

A save that fails a check is refused with the reason. A change reaches checkouts
on the replica that saved it at once and on the others within **60 s**; no
deploy is needed. A price with no variant cannot be bought (`/api/public/pricing`
shows it `available=false`) and is listed in Harbor's attention list as
`unmapped_price`. Publishing a tier version never waits for its mapping.

A tier can carry the **free month** only if every one of its mapped variants has
a one-month free trial configured in LS. Checkouts skip that trial (`skip_trial`)
unless the tier is the one marked in Harbor and the buyer's account has not had a
free month.

`LEMONSQUEEZY_VARIANTS` is deprecated. When it is set and the current mode's map
is empty, boot imports it once (rows marked `env-seed`); afterwards it is ignored
with a warning. Remove it from the env once the import is done.

## 1. Set up the test store

1. In LS test mode create the products and one variant per price row (monthly,
   yearly). Prices are **tax-exclusive** and must equal the price rows' `net_minor`.
2. On the variants of every tier you may want to offer with a free month, enable
   a free trial of **1 month**. Other variants: no trial.
3. Add a webhook to `https://<api>/api/webhooks/lemonsqueezy` with a new signing
   secret and these events: `subscription_created`, `subscription_updated`,
   `subscription_cancelled`, `subscription_resumed`, `subscription_expired`,
   `subscription_paused`, `subscription_unpaused`,
   `subscription_payment_success`, `subscription_payment_failed`,
   `subscription_payment_recovered`, `subscription_payment_refunded`,
   `order_created`, `order_refunded`.
4. In the customer portal settings, **turn plan changes off**. Ogen owns plan
   changes; if one slips through, the webhook reconciles the tier.
5. In Harbor, publish the purchasable tier versions (Pro, Max) with their prices,
   and retire the €0 `trial-v1` version.
6. Set the env (`BILLING_PROVIDER=lemonsqueezy`, `LEMONSQUEEZY_TEST_MODE=true`,
   store id) and deploy. Set the test API key and webhook secret in Harbor →
   Secrets.
7. In **Harbor → Billing → Variant mapping** (test), accept the suggestions or pick
   each price's variant. Every row should read *Matches*.
8. Check Harbor → Billing settings shows *configured*. Boot logs a warning for a
   missing secret or an empty variant map instead of failing.
9. In Harbor, mark the free-month tier (`SetFreeTrialTier`) if you want the
   offer during the test period.

A new tier version later on needs no deploy: create its variants in LS, publish
the version, and map its prices in Harbor.

While in test mode anyone can "pay" with the test card `4242 4242 4242 4242`.
Those subscriptions are invalidated at the switch.

## 2. Rehearse on staging (test mode)

- Sign up (password and Google): the workspace answers 402 `billing_required`
  everywhere outside the plan and billing routes.
- `/signup?plan=<tier>` with the marked tier: checkout charges $0 and the
  subscription is `on_trial` with `trial_ends_at` one month out.
- The same account's second workspace checks out without the free month.
- Upgrade (charged now with proration), downgrade (scheduled for the renewal),
  keep the current plan (cancels it), open the portal.
- Decline card `4000 0000 0000 0002` at renewal: the workspace becomes `past_due`,
  owners get a notification and an email, Harbor lists it, nothing else changes.
- Let a trial expire without paying: with `BILLING_TRIAL_TEARDOWN_LIVE=false` it is
  only listed in Harbor (`expired_never_paid`, `dry_run`). Turn the flag on and
  repeat: the Zernio profile is torn down and the accounts show disconnected.

## 3. Switch to live

1. **Activate the store** in LS (LS reviews it).
2. **Copy to Live Mode** for every product and variant, including the trial
   settings.
3. In live mode: add the webhook (same URL, a **new** signing secret, the same
   events), turn plan changes off in the portal, keep prices tax-exclusive, keep
   the 1-month trial only on the trial-capable variants.
4. **Prepare the live map** before the window: set the live API key as
   `lemonsqueezy_api_key_live` in Harbor → Secrets, then in **Variant mapping**
   switch to *Live*, accept the suggestions and check every row reads *Matches*.
   Without that key the live map can still be filled in, but stays *Not checked*
   until billing runs live (the boot check then logs any mismatch).
5. In one maintenance window, back to back:
   1. In **Harbor → Secrets**, set the live `lemonsqueezy_api_key` and
      `lemonsqueezy_webhook_secret`. The replica that served the change uses them
      at once; the others within **60 s** (secret cache).
   2. In the **env**, set `LEMONSQUEEZY_TEST_MODE=false`, then deploy. The
      restart clears every replica's secret cache and switches every replica to
      the live map.

   Until both are done the key and the mode disagree: webhooks from the other
   mode are acknowledged and ignored, and a webhook signed with the new secret
   that reaches a replica still holding the old one gets 401 (LS retries for
   about 2.5 minutes).
6. When the deploy is healthy, run **ResyncSubscription** in Harbor for any
   workspace that checked out during the window, or wait one reconcile tick
   (`BILLING_RECONCILE_INTERVAL`).
7. In Harbor, run **InvalidateTestSubscriptions** with *dry run* and check the
   list holds exactly the test-period workspaces. Then run it for real. Each
   test-mode subscription is invalidated (`mode_cutover`), its workspace goes
   back behind the gate (when it was gated), and its owners get an in-app notice
   and the `billing_reentry_required` email. Their Zernio profiles are kept. The
   RPC refuses while `LEMONSQUEEZY_TEST_MODE=true`.
8. In Harbor, **SetFreeTrialTier** for the launch offer (no tier is marked by
   default) and share `/signup?plan=<tier_id>`.
9. Smoke test: a real card on a real workspace, then refund it from the LS
   dashboard.

## Operating notes

- Dunning is the operator's call. `past_due`, `unpaid`, `paused` and `expired`
  never change a tier or a workspace status on their own; Harbor's attention list
  shows them, with suspend / move tier / comp as the levers.
- A trial that expires without any payment tears down the workspace's Zernio
  profile (when `BILLING_TRIAL_TEARDOWN_LIVE=true`). Nothing else is removed.
- `SetBillingRequired(false)` comps a workspace: it can use the app without
  checking out.
- The webhook log (`billing_webhook_events`) keeps 90 days; a delivery that
  failed is re-read by the reconcile sweep for 24 hours.
