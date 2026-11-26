# Billing: Polar setup, the provider switch and the move to live payments

Polar is Ogen's merchant of record. Billing runs against the Polar **sandbox**
until the production organization is verified, then switches to **production**
in one maintenance window. The sandbox and production are separate APIs
(`sandbox-api.polar.sh` / `api.polar.sh`) with separate organizations, tokens,
products and webhook endpoints, so the switch touches the secret store, the env,
the product map and every subscription made in the sandbox.

Settings involved:

| Where | Name | Sandbox period | Production |
| -- | -- | -- | -- |
| env | `BILLING_PROVIDER` | `polar` | `polar` |
| env | `BILLING_TEST_MODE` | `true` | `false` |
| env | `POLAR_ORGANIZATION_ID` | sandbox organization id | production organization id |
| env | `POLAR_ORGANIZATION_ID_LIVE` | production organization id (optional, to prepare the live map) | not needed |
| env | `POLAR_API_VERSION` | `2026-10` | `2026-10` |
| env | `BILLING_TRIAL_TEARDOWN_LIVE` | `false`, then `true` once verified | `true` |
| Harbor → Secrets | `polar_access_token` | sandbox Organization Access Token | production token |
| Harbor → Secrets | `polar_webhook_secret` | sandbox endpoint's secret | production endpoint's secret |
| Harbor → Secrets | `polar_access_token_live` | production token, `products:read` only (optional) | not needed |
| Harbor → Billing → Product mapping | sandbox map | sandbox product ids | (kept, unused) |
| Harbor → Billing → Product mapping | live map | prepared ahead | production product ids |

`BILLING_PROVIDER=lemonsqueezy`, the provider before Polar, turns billing off and
boot logs an error saying so: the release can be deployed before the env names
Polar. Never set `BILLING_PROVIDER=polar` on a release older than the Polar one:
that release refuses to boot.

## Tokens

Create each Organization Access Token in the Polar dashboard with the longest
expiry offered, and put the expiry in the calendar: an expired token makes
Harbor show *not configured* and every billing call fail.

- Running mode (`polar_access_token`): `checkouts:write`, `subscriptions:read`,
  `subscriptions:write`, `products:read`, `customers:read`,
  `customer_sessions:write`, `orders:read`, `refunds:read`.
- `polar_access_token_live`: `products:read` only.

river-service leases `polar_access_token` for the reconcile and payments jobs,
and sends them to the sandbox or production API by the upstream ogen names
(`polar_sandbox` / `polar`).

## The product map

Each sellable price row (a non-zero price of a published, purchasable tier
version) is sold through one Polar product. The map is kept in the database, per
mode, and edited in **Harbor → Billing → Product mapping** (`BillingAdminService`
`ListProductMappings` / `SetProductMapping` / `SuggestProductMappings`). Products
are made in the Polar dashboard; the rest is checked by Ogen:

- the product must be a recurring product of that mode's organization, not
  archived, and bill **every month or every year** like the price row (a Polar
  product carries one interval, so monthly and yearly are separate products);
- it must have a fixed price in the row's **currency**, equal to the row's
  `net_minor`, **excluding tax**;
- on the free-month tier, it must carry a **1-month free trial**;
- one product sells one price per currency and mode (a product priced in EUR
  and USD can sell one EUR row and one USD row).

A save that fails a check is refused with the reason. A change reaches checkouts
on the replica that saved it at once and on the others within **60 s**; no
deploy is needed. A price with no product, or whose product was archived, cannot
be bought (`/api/public/pricing` shows it `available=false`) and is listed in
Harbor's attention list as `unmapped_price`.

A tier can carry the **free month** only if every one of its mapped products has
a one-month free trial. Checkouts turn the trial off (`allow_trial=false`)
unless the tier is the one marked in Harbor and the buyer's account has not had a
free month with Polar. Discount codes are never offered. Turn on **Prevent trial
abuse** in the organization settings as a second check.

## 1. Set up the sandbox organization

Use one sandbox organization per environment (development, production while it
is in the sandbox): a webhook endpoint receives every event of its
organization, and another environment's events would show up as unresolved.

1. In the sandbox, create one product per price row and interval, for example
   *Pro monthly* and *Pro yearly*, each with a fixed price per currency sold,
   **tax-exclusive**, equal to the price rows' `net_minor`.
2. On the products of every tier you may offer with a free month, set a free
   trial of **1 month**. Other products: no trial.
3. Create the token (scopes above) and note the organization id.
4. Set the env (`BILLING_PROVIDER=polar`, `BILLING_TEST_MODE=true`,
   `POLAR_ORGANIZATION_ID`) and deploy. Set `polar_access_token` in Harbor →
   Secrets.
5. Only now add the webhook endpoint (Polar disables an endpoint after ten
   failed deliveries in a row, so it must point at a release that serves it):
   `https://<api>/api/webhooks/polar`, format JSON, API version `2026-10`,
   events `subscription.created`, `subscription.active`, `subscription.updated`,
   `subscription.canceled`, `subscription.uncanceled`, `subscription.revoked`,
   `subscription.past_due`, `subscription.paused`, `subscription.resumed`,
   `order.paid`, `order.updated`, `order.refunded`, `refund.created`,
   `refund.updated`, `checkout.expired`. Set its secret as
   `polar_webhook_secret` and send a test event: it should get 2xx.
6. In **Harbor → Billing → Product mapping** (sandbox), accept the suggestions or
   pick each price's product. Every row should read *Matches*.
7. Check Harbor → Billing settings shows *configured*. Boot logs a warning for a
   missing secret or an empty product map instead of failing.
8. In Harbor, mark the free-month tier (`SetFreeTrialTier`) if you want the
   offer during the sandbox period.

A new tier version later on needs no deploy: create its products in Polar,
publish the version, and map its prices in Harbor.

While in the sandbox anyone can "pay" with the test card `4242 4242 4242 4242`.
Those subscriptions are invalidated at the switch to production.

## 2. The switch from Lemon Squeezy

Workspaces that checked out with Lemon Squeezy keep their subscription rows as
history. Until the switch they still lift the billing gate, but Ogen does not
change or re-read them. Once Polar is configured:

1. In Harbor, run **InvalidateSubscriptions** with scope *Provider switch* and
   *dry run*, and check the list holds exactly the Lemon Squeezy workspaces.
2. Run it for real. Each Lemon Squeezy subscription is invalidated
   (`provider_switch`), its open scheduled change cancelled, its workspace put
   behind the gate (a complimentary one never is), and its owners get an in-app
   notice and the `billing_reentry_required` email. Their Zernio profiles are
   kept. A second run finds nothing.
3. A free month taken with Lemon Squeezy does not count against the Polar one.
4. Turn the Lemon Squeezy webhook off in the Lemon Squeezy dashboard.

## 3. Rehearse on staging (sandbox)

- Sign up (password and Google): the workspace answers 402 `billing_required`
  everywhere outside the plan and billing routes.
- `/signup?plan=<tier>` with the marked tier: the checkout takes the card,
  charges nothing, and the subscription is `on_trial` with `trial_ends_at` one
  month out.
- The same account's second workspace checks out without the free month.
- Upgrade (charged now with proration; a declined card answers 409
  `payment_failed` and changes nothing), downgrade (Polar holds it for the
  renewal; Harbor's tenant card shows the pending product), keep the current plan
  (drops it), open the portal.
- At the renewal the downgrade lands with no job: the renewal's snapshot moves
  the tier. A downgrade Polar did not apply is failed by the reconcile sweep an
  hour after its date and listed as `scheduled_change_failed`.
- A failed renewal: the workspace becomes `past_due`, owners get a notification
  and an email, Harbor lists it, nothing else changes. Polar retries over 21 days
  before the subscription becomes `unpaid`.
- Let a trial expire without paying: with `BILLING_TRIAL_TEARDOWN_LIVE=false` it is
  only listed in Harbor (`expired_never_paid`, `dry_run`). Turn the flag on and
  repeat: the Zernio profile is torn down and the accounts show disconnected.
- Payments: after the `billing_sync_payments` run, Harbor P&L lists the orders,
  each refund as its own row, a EUR payment converted at the ECB rate of its day
  and a USD one at 1.

## 4. Switch to production

1. Finish the production organization in Polar: identity verification and the
   payout account.
2. Create the production products (as in the sandbox) and both tokens.
3. **Prepare the live map** before the window: set `polar_access_token_live` in
   Harbor → Secrets and `POLAR_ORGANIZATION_ID_LIVE` in the env, then in
   **Product mapping** switch to *Production*, accept the suggestions and check
   every row reads *Matches*. Without them the live map can still be filled in,
   but stays *Not checked* until billing runs live (the boot check then logs any
   mismatch).
4. In one maintenance window, back to back:
   1. Add the production webhook endpoint (same URL, events and version) and,
      in **Harbor → Secrets**, set the production `polar_access_token` and
      `polar_webhook_secret`. The replica that served the change uses them at
      once; the others within **60 s** (secret cache).
   2. In the **env**, set `BILLING_TEST_MODE=false` and `POLAR_ORGANIZATION_ID`
      to the production organization, then deploy. The restart clears every
      replica's secret cache and switches every replica to the live map.

   Until both are done the token and the mode disagree, and a delivery signed
   with the new secret that reaches a replica still holding the old one gets 401
   (Polar retries it).
5. When the deploy is healthy, run **ResyncSubscription** in Harbor for any
   workspace that checked out during the window, or wait one reconcile tick
   (river-service's `BILLING_RECONCILE_INTERVAL`).
6. In Harbor, run **InvalidateSubscriptions** with scope *Mode cutover* and *dry
   run*, check the list, then run it for real: the sandbox subscriptions are
   invalidated (`mode_cutover`) as in step 2. It refuses while
   `BILLING_TEST_MODE=true`.
7. Remove the sandbox webhook endpoint.
8. In Harbor, **SetFreeTrialTier** for the launch offer (no tier is marked by
   default) and share `/signup?plan=<tier_id>`.
9. Smoke test: a real card on a real workspace, then refund it from the Polar
   dashboard.

## Operating notes

- Dunning is the operator's call. `past_due`, `unpaid`, `paused` and `expired`
  never change a tier or a workspace status on their own; Harbor's attention list
  shows them, with suspend / move tier / comp as the levers.
- A trial that expires without any payment tears down the workspace's Zernio
  profile (when `BILLING_TRIAL_TEARDOWN_LIVE=true`). Nothing else is removed.
- `SetBillingRequired(false)` comps a workspace: it can use the app without
  checking out.
- Polar disables a webhook endpoint after ten failed deliveries in a row. A
  delivery Ogen cannot apply is answered with an error for an hour, then
  acknowledged and left to the reconcile sweep, so one bad event does not
  disable the endpoint. If it was disabled anyway, re-enable it and resend from
  the Polar dashboard; the reconcile sweep has converged the mirror meanwhile.
- The webhook log (`billing_webhook_events`) keeps 90 days; a delivery that
  failed is re-read by the reconcile sweep for 24 hours.
- Credit from a downgrade, if any arises, stays in the customer's Polar balance:
  Polar does not refund it to the card.
- Non-USD payments are converted at the ECB reference rate of their day; until
  that rate is fetched (by `billing_sync_payments`) their USD amounts are empty
  and Harbor skips them. The actual payout rate is reconciled separately.
