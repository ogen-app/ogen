# Billing: sandbox setup and the switch to live payments

Lemon Squeezy (LS) is Ogen's merchant of record. Billing runs in LS **test mode**
until the store is activated, then switches to **live** in one maintenance window.
Test and live have separate API keys, separate products and variants (different
ids) and separate webhooks, so the switch touches the secret store, the env and
every subscription made in test mode.

Settings involved:

| Where | Name | Test period | Live |
| -- | -- | -- | -- |
| env | `BILLING_PROVIDER` | `lemonsqueezy` | `lemonsqueezy` |
| env | `LEMONSQUEEZY_TEST_MODE` | `true` | `false` |
| env | `LEMONSQUEEZY_STORE_ID` | store id | same store id |
| env | `LEMONSQUEEZY_VARIANTS` | test variant ids | live variant ids |
| env | `BILLING_TRIAL_TEARDOWN_LIVE` | `false`, then `true` once verified | `true` |
| Harbor → Secrets | `lemonsqueezy_api_key` | test key | live key |
| Harbor → Secrets | `lemonsqueezy_webhook_secret` | test signing secret | live signing secret |

`LEMONSQUEEZY_VARIANTS` maps each tier version price row to the LS variant that
sells it, e.g. `{"price-pro-m":"123456","price-pro-y":"123457"}`. A tier can carry
the **free month** only if every one of its mapped variants has a one-month free
trial configured in LS. Checkouts skip that trial (`skip_trial`) unless the tier
is the one marked in Harbor and the buyer's account has not had a free month.

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
   store id, test variant map) and deploy. Set the test API key and webhook secret
   in Harbor → Secrets.
7. Check Harbor → Billing settings shows *configured*. Boot logs a warning for a
   missing secret or an empty variant map instead of failing.
8. In Harbor, mark the free-month tier (`SetFreeTrialTier`) if you want the
   offer during the test period.

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
   settings. Write down the new live variant ids.
3. In live mode: add the webhook (same URL, a **new** signing secret, the same
   events), turn plan changes off in the portal, keep prices tax-exclusive, keep
   the 1-month trial only on the trial-capable variants.
4. In one maintenance window, back to back:
   1. In **Harbor → Secrets**, set the live `lemonsqueezy_api_key` and
      `lemonsqueezy_webhook_secret`. The replica that served the change uses them
      at once; the others within **60 s** (secret cache).
   2. In the **env**, set `LEMONSQUEEZY_TEST_MODE=false` and the live
      `LEMONSQUEEZY_VARIANTS`, then deploy. The restart clears every replica's
      secret cache.

   Until both are done the key and the mode disagree: webhooks from the other
   mode are acknowledged and ignored, and a webhook signed with the new secret
   that reaches a replica still holding the old one gets 401 (LS retries for
   about 2.5 minutes).
5. When the deploy is healthy, run **ResyncSubscription** in Harbor for any
   workspace that checked out during the window, or wait one reconcile tick
   (`BILLING_RECONCILE_INTERVAL`).
6. In Harbor, run **InvalidateTestSubscriptions** with *dry run* and check the
   list holds exactly the test-period workspaces. Then run it for real. Each
   test-mode subscription is invalidated (`mode_cutover`), its workspace goes
   back behind the gate (when it was gated), and its owners get an in-app notice
   and the `billing_reentry_required` email. Their Zernio profiles are kept. The
   RPC refuses while `LEMONSQUEEZY_TEST_MODE=true`.
7. In Harbor, **SetFreeTrialTier** for the launch offer (no tier is marked by
   default) and share `/signup?plan=<tier_id>`.
8. Smoke test: a real card on a real workspace, then refund it from the LS
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
