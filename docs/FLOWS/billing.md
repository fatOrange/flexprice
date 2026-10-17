# Flow: Billing

## Trigger

Billing is **scheduled and event-driven**:

- **Temporal schedules** instantiated at worker startup (`EnsureSchedules`).
- Cron-style HTTP stubs under **`/v1/cron/...`** (documented as secondary to Temporal automation).
- State transitions originating from **`SubscriptionService`** / **`BillingService`** as subscription periods advance, usage aggregates finalize, coupons apply, commitments accrue.

## Execution path (conceptual aggregation)

Billing threads multiple engines:

1. **Subscription-period evaluation** selects applicable price components (seat, tiered meter, addons, commitments) — anchored in **`internal/ee/service/billing.go`** (and specialized files like billing meter usage splits).
2. **Usage retrieval** leverages ClickHouse analytics repositories + feature trackers previously populated (`event-processing` pipeline).
3. **Proration & tax** calculators (`domain/proration` + tax services).
4. **Invoice materialization**: line items aggregated, persisted via Ent invoice repositories (**`BillingService`** + **`InvoiceService`** interplay).
5. **Payment intents / processor routing** delegated to integrations via **`IntegrationFactory`** and payment processor service.

Temporal workflows encapsulate risky multi-step arcs (billing runs, integrations sync) invoked from deeper service methods (`GetGlobalTemporalService` hotspots remain).

## Modules touched

- Core: `internal/ee/service/billing*.go`, `subscription.go`, `invoice.go`, `payment*.go`
- Temporal: workflows under `internal/temporal/workflows/subscription`, `invoice`, `cron`
- Domain: pricing, entitlement, invoice, wallet, coupons, addons
- Integrations when external sync required post-billing milestones

## Database operations

- Heavy **PostgreSQL** writes: invoices + line items, subscription phase transitions, entitlement quantities, ledger-like wallet movements (depending on product path).
- **ClickHouse reads** for usage shaping.

## External systems

- PSP connectors (Stripe, Razorpay, Paddle, Moyasar, Nomod, WeChat Pay, etc.) downstream of invoice finalization cues.
- Optional accounting/export connectors post invoice states.

## Async operations

Temporal workflows retry activities; webhook publisher emits post-state notifications.

## Failure points

| Area | Impact |
| ---- | ------- |
| Idempotency lapses around period boundaries | Double invoice risk |
| ClickHouse staleness vs PG subscription clock | Incorrect rated quantities |
| Integration timeouts | Paid state inconsistent with PSP |
| Massive billing single-file complexity | Regression risk on localized edits |

## Retry behavior

- Temporal activities configurable per registration.
- PSP operations often implement internal retry wrappers (consult specific integration packages).

## State transitions

Representative coarse states (consult domain enums/models for authoritative lists):

```
draft invoice → finalized → paid / void / credited
wallet balance adjusts with applications of credits/grants alongside invoice settlement
subscriptions advance billing periods (active ↔ paused ↔ cancelled variants)
```

## WeChat Pay existing-invoice payments

The WeChat Pay connector settles **manual one-off invoice payments** for existing invoices
through `POST /v1/payments`; it does not create subscriptions, and checkout sessions reject
`wechatpay` as their payment provider before any write. Only Native (desktop QR) and JSAPI
(inside the WeChat browser) exist: no H5/app/mini-program channel, wallet top-up, autocharge,
mandate, or login/OAuth. The calling business app creates the invoice with the existing APIs,
pays it by invoice ID, dedupes on its own invoice/payment id, and owns all downstream
entitlement (e.g. monthly card periods and early renewals); FlexPrice itself only settles the
invoice.

### Connection (tenant + environment scoped)

Create a `wechatpay` connection through the existing connection API. Fields are sent as
plaintext in `encrypted_secret_data`; the server encrypts `merchant_private_key` and
`api_v3_key` before persisting, and **connection responses never return secret material**:

```json
POST /v1/connections
{
  "name": "WeChat Pay",
  "provider_type": "wechatpay",
  "encrypted_secret_data": {
    "appid": "wx_example_appid",
    "mchid": "1900000001",
    "merchant_certificate_serial_number": "EXAMPLE_CERT_SERIAL",
    "merchant_private_key": "<merchant private key PEM>",
    "api_v3_key": "<32-character APIv3 key>",
    "platform_public_key": "<WeChat Pay platform public key PEM>",
    "platform_public_key_id": "PUB_KEY_ID_EXAMPLE",
    "notify_url": "https://api.example.com/v1/webhooks/wechatpay"
  }
}
```

- `notify_url` is the public **https base** of the callback route: path required, no query,
  fragment, or userinfo, and no loopback/private host. FlexPrice appends
  `/{tenant_id}/{environment_id}` when creating each order or refund, so one connection
  serves exactly one environment.
- Updates accept the same flat keys or a nested `{"wechatpay": {...}}` object; credentials are
  decrypted per request, so a rotation applies to the next call.

### Creating a payment

`POST /v1/payments` with `destination_type=INVOICE` (the only supported destination for this
gateway), `payment_method_type=PAYMENT_LINK`, `payment_gateway=wechatpay`, CNY, and an amount
equal to the invoice's outstanding amount (positive, whole fen):

```json
{
  "destination_type": "INVOICE",
  "destination_id": "inv_…",
  "payment_method_type": "PAYMENT_LINK",
  "payment_gateway": "wechatpay",
  "amount": "100.00",
  "currency": "CNY",
  "process_payment": true,
  "gateway_options": { "wechatpay": { "payment_type": "native" } }
}
```

JSAPI adds `"payment_type": "jsapi"` and `"openid"` — the payer's openid must come from the
same WeChat app (`appid`) as the connection, collected by the caller's own backend login;
FlexPrice performs no OAuth. The response is a payment with a typed `next_action` and the
persisted deadline (`expires_at`, fixed at 30 minutes and never extended):

```json
{
  "id": "pay_…",
  "payment_gateway": "wechatpay",
  "payment_status": "PENDING",
  "amount": "100.00",
  "currency": "CNY",
  "next_action": {
    "type": "qr_code",
    "qr_content": "weixin://wxpay/bizpayurl?pr=…"
  },
  "expires_at": "2026-10-12T16:04:34Z"
}
```

- Native renders `next_action.qr_content` as a QR code the customer scans in the WeChat app;
  JSAPI returns signed `next_action.jsapi` parameters (`appId`, `timeStamp`, `nonceStr`,
  `package`, `signType`, `paySign`) for the front end to pass to `WeixinJSBridge`. The bridge
  call is a frontend action only — never proof of payment.
- FlexPrice admits at most one open WeChat Pay order per invoice: a repeated create on the
  same channel returns the live intent, a channel switch is refused, and an explicit
  `idempotency_key` resolves your own retries to the existing payment.

### Settlement

- Only a **signed callback** or a **signed query** settles anything; the invoice stays unpaid
  until that authoritative confirmation (see
  [`webhook-processing.md`](webhook-processing.md#wechat-pay-callbacks-payments-and-refunds)).
- Admission and settlement both take the invoice row lock: creating the intent and applying a
  verified transaction serialize on the invoice, so concurrent deliveries cannot
  double-collect or leave a paid payment unreconciled. Settlement runs as one transaction
  across the payment transition, its attempt, and invoice reconciliation.
- The provider order is payable for 30 minutes (`time_expire`). Past the deadline the
  reconciler queries the order again; if it is still unpaid it attempts a close. WeChat
  refuses to close a paid order (`ORDERPAID`), so a follow-up signed query settles a payment
  that landed just before expiry instead of expiring it; a confirmed closed/revoked/errored
  order expires the local intent. An unreadable or still-ambiguous close/query outcome leaves
  the payment untouched for a later reconciliation — uncertainty is never turned into a local
  failure or settlement.
- `GET /v1/payments/:id` runs the same reconciler on in-flight WeChat Pay payments, so a
  dropped callback converges on the next read.
- Outbound events on this path come from the existing payment/invoice/refund publishers,
  which queue the publish behind the surrounding transaction's commit (post-commit hooks) —
  no new delivery pipeline.

### Refunds

- Only the **full original transaction** can be returned, to the original source; a partial
  refund is rejected while the credit note is finalized, before any refund row or provider
  call exists.
- Refunds originate from the existing credit-note flow: finalize a credit note with
  `{"refund_target": "BACK_TO_SOURCE"}`. WeChat Pay accepts refunds asynchronously, so the
  gateway refund row moves to `PROCESSING` on acceptance; a signed `REFUND.SUCCESS` callback
  or `GET /v1/refunds/:id` (signed query fallback) settles it to `SUCCEEDED` at most once.
- A provider-confirmed `ABNORMAL`/`CLOSED` refund fails the row; there is **no automatic
  wallet fallback** for WeChat Pay refunds, and retrying a failed WeChat Pay refund is
  rejected — resolve it at the merchant platform. Until a verified result arrives the row
  stays `PROCESSING`: an unreadable or still-processing query is an unknown outcome, not a
  failure.

### Local acceptance and contract

`make wechatpay-smoke` (`go run ./cmd/wechatpay-smoke`) boots the real router, services, and
official WeChat Pay SDK against `cmd/wechatpay-smoke/wechatsim` — an in-memory, signed
simulation of the APIv3 gateway that mints obviously fake credentials per run. It validates
the Native/JSAPI actions and their signatures, callback verification, expiry with
query-before-close, full-refund settlement, and event emission; a passing run prints
`wechatpay smoke PASSED`. This is a local contract test only: no real merchant account, no
official WeChat sandbox, and no live PostgreSQL or production infrastructure are involved.

The REST shapes above are defined by the API and reflected in the OpenAPI spec under
`docs/swagger/` (regenerate with `make swagger`, validate with `make speakeasy-validate`).
Publishing regenerated SDK/MCP clients stays with the existing release tooling
(`make sdk-all`; locally, `make sdk-all-local` loads `SPEAKEASY_API_KEY` from `.secrets`).

## Related flows

- [invoice-lifecycle.md](invoice-lifecycle.md)
- [subscription-lifecycle.md](subscription-lifecycle.md)
