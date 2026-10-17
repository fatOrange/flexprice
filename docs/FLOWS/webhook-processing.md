# Flow: Webhook processing

Two distinct webhook families coexist:

(A) **Inbound provider webhooks** (Stripe/Razorpay/Paddle/etc.)  
(B) **Outbound customer webhooks** (FlexPrice → subscriber endpoints via Svix/Kafka-backed pipeline)

## A — Inbound payment/provider webhooks

### Trigger

Provider posts signed HTTP callbacks to **`/v1/webhooks/...`** (route groups declared in router; multiplexed through `WebhookHandler` in `internal/api/v1/webhook.go`).

### Execution path

1. Handler authenticates signatures per integration package (`integration/*/webhook`).
2. Event normalized into internal DTO/domain operations (payments, invoices, subscription mirrors).
3. Services persist authoritative changes in Postgres and may enqueue async follow-ups.

### WeChat Pay callbacks (payments and refunds)

`POST /v1/webhooks/wechatpay/{tenant_id}/{environment_id}` (`internal/api/v1/wechatpay.go`)
serves both payment and refund notifications for this connector; the path segments scope the
request to a tenant/environment before any lookup, and the route carries no auth middleware.

1. The raw body is bounded (1 MiB) and passed to `internal/ee/service/wechatpay*.go`, which
   delegates verification to `internal/integration/wechatpay`: the WeChat platform signature
   is verified against the connection's platform public key and the notification resource is
   decrypted with the APIv3 key (AES-256-GCM) via the official SDK. Nothing in the body is
   trusted before that.
2. Every verified field is then checked against local state — merchant and app against the
   tenant's connection, order/transaction/refund ids, currency, and amounts against the local
   payment or refund row. A mismatch is a terminal rejection with no mutation.
3. `TRANSACTION.SUCCESS` settles the payment through the same path as the status query
   (`PaymentLifecycle` + invoice reconciliation); `REFUND.SUCCESS` settles and
   `REFUND.ABNORMAL`/`REFUND.CLOSED` fail the canonical refund row through `RefundService`
   (an `ABNORMAL`/`CLOSED` refund has no wallet fallback and cannot be retried, so it is
   resolved at the merchant platform). Redeliveries hit the same guards and settle at most
   once. Refund notifications use the same notify URL as payments.
4. Success is an empty `200`, returned **only after** the effect is durably recorded;
   validation rejections return 400 and unknown/foreign scope or transient failures return
   500, so WeChat Pay redelivers. Other verified event types (including `TRANSACTION.CLOSED`,
   which the query path resolves) are acknowledged without state changes.

### Modules touched

- `internal/api/v1/webhook.go`
- Integration packages (`internal/integration/*/webhook`, `paddle`, `stripe`, `quickbooks`, `zoho`, `razorpay`, `nomod`, `moyasar`, `chargebee`, `hubspot` where applicable).
- WeChat Pay callbacks: `internal/api/v1/wechatpay.go`, `internal/ee/service/wechatpay*.go`, `internal/integration/wechatpay` (official `wechatpay-go` SDK).

### Failure points & retries

Depends on PSP expectations—typically return non-200 on transient failure so remote side retries exponential backoff automatically.

Malformed signatures → rejection before service layer.

---

## B — Outbound system/customer webhooks

### Trigger

Internal domain milestones enqueue messages onto **system-events Kafka topics** configured under webhook sections (`cfg.Webhook`). Consumer **`internal/webhook`** (Fx module wired before generic services ordering) listens & processes.

### Execution path

1. **WebhookService.RegisterHandler** hooks into **`pubsub.Router`** when processing mode allows (`includeProcessingHandlers` true in deployment modes exercising consumers).
2. **Payload factories** hydrate JSON from many read-services (see [`internal/webhook/module.go`](../../internal/webhook/module.go) injecting invoice/plan/feature/subscription/etc. services).
3. **WebhookPublisher** (backed by shared Kafka producer) emits final envelope (potential persistence of `systemevent` aggregates — follow `publisher` package interplay with `repository/ent/SystemEvent`).
4. **Svix integration** optionally delivers routed events (see Svix handlers in webhook API endpoints for dashboard onboarding).

Event publishers invoked inside a write transaction (payment, invoice, refund, subscription, and wallet transaction paths) queue the publish through the transaction's post-commit hooks (`types.RegisterPostCommit`) and hand it to the publisher only after that transaction commits; with no transaction open the publish is immediate. The same mechanism defers `DispatchMarkPaid`'s provider notifications (the mark-paid workflows a paid invoice triggers for connected providers). A delivered event therefore never describes rolled-back state, and queued work never reads uncommitted rows.

### Async operations

Entire outbound path asynchronous — HTTP API returns before delivery confirmation semantics unless awaiting internal publish only.

### Failure points

Poison queue / DLQ if repeated processing failure at Watermill middleware layer.

Stale payload blueprint if parallel API evolution not mirrored in builder tests.

Misconfigured consumer groups causing duplicate webhook fan-out.

---

## Retry behavior references

Outbound side inherits global Watermill Retry + DLQ semantics documented in [`retry-handling.md`](retry-handling.md).

Inbound side primarily relies on PSP resend semantics.

---

## Related flows

- [event-processing.md](event-processing.md) (shared router infrastructure)
- [api-request-lifecycle.md](api-request-lifecycle.md)
