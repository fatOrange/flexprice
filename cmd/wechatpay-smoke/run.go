package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/flexprice/flexprice/internal/api/dto"
	"github.com/flexprice/flexprice/internal/integration/wechatpay"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/shopspring/decimal"

	wechatsim "github.com/flexprice/flexprice/cmd/wechatpay-smoke/wechatsim"
)

// smokeRun executes the acceptance scenarios and collects every failed check.
type smokeRun struct {
	stack    *smokeStack
	failures []string
}

func newSmokeRun(stack *smokeStack) *smokeRun { return &smokeRun{stack: stack} }

func (r *smokeRun) execute() error {
	ctx := context.Background()

	native := r.scenarioNativePayment(ctx)
	r.scenarioCallbackValidation(ctx, native)
	r.scenarioJSAPI(ctx)
	r.scenarioExpiryRace(ctx)
	r.scenarioRefunds(ctx, native)
	r.scenarioOutboundEvents(ctx)
	r.scenarioIntegrationConfig(ctx)

	if len(r.failures) > 0 {
		return fmt.Errorf("%d check(s) failed:\n- %s", len(r.failures), strings.Join(r.failures, "\n- "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 1: Native QR creation, authoritative-receipt timing, duplicate query.
// ---------------------------------------------------------------------------

type paymentFixture struct {
	invoiceID  string
	lineItemID string
	payment    *dto.PaymentResponse
	orderID    string
}

func (r *smokeRun) scenarioNativePayment(ctx context.Context) *paymentFixture {
	r.section("1. Native payment: creation, signed order, unpaid until authoritative confirmation")

	fixture := r.createPaymentFixture(ctx, "100.00", nativeOptions())

	if fixture != nil {
		order := r.stack.sim.Order(fixture.orderID)
		r.expect(order != nil, "simulated gateway received the native order %s", fixture.orderID)
		if order != nil {
			r.expect(order.AmountFen == 10000, "order carries exactly 10000 fen, got %d", order.AmountFen)
			r.expect(order.Currency == "CNY", "order currency is CNY, got %s", order.Currency)
			r.expect(order.TradeType == "NATIVE", "order trade type is NATIVE, got %s", order.TradeType)
			r.expect(order.MchID == r.stack.fixture.MerchantID && order.AppID == r.stack.fixture.AppID,
				"order is bound to the connection's merchant %s / app %s", order.MchID, order.AppID)
			r.expect(order.TimeExpire.After(time.Now().Add(29*time.Minute)) && order.TimeExpire.Before(time.Now().Add(31*time.Minute)),
				"order time_expire is ~30 minutes out, got %s", order.TimeExpire.Format(time.RFC3339))
			r.expect(strings.HasSuffix(order.NotifyURL, "/v1/webhooks/wechatpay/"+smokeTenantID+"/"+smokeEnvID),
				"order notify_url routes back to this tenant/environment, got %s", order.NotifyURL)
			r.expect(order.NativeCodeURL != "" && strings.HasPrefix(order.NativeCodeURL, "weixin://"),
				"order produced a WeChat QR payload, got %q", order.NativeCodeURL)
		}

		r.expectEveryRequestSigned("native order")

		// The invoice must remain unpaid: the frontend action is never a receipt.
		inv := r.getInvoice(ctx, fixture.invoiceID)
		r.expect(inv != nil && inv.PaymentStatus != types.PaymentStatusSucceeded,
			"invoice stays unpaid after a QR is handed out (status %v)", invoicePaymentStatus(inv))

		// Duplicate status query before payment: still pending, invoice untouched.
		before := r.stack.sim.QueryCalls(fixture.orderID)
		queried := r.getPayment(ctx, fixture.payment.ID)
		r.expect(queried != nil && queried.PaymentStatus == types.PaymentStatusPending,
			"query before payment keeps the payment PENDING, got %v", paymentStatus(queried))
		r.expect(r.stack.sim.QueryCalls(fixture.orderID) > before,
			"query path actually called the gateway (queries %d -> %d)", before, r.stack.sim.QueryCalls(fixture.orderID))
		r.expect(r.stack.sim.CloseCalls(fixture.orderID) == 0,
			"no close is attempted before expiry (close calls %d)", r.stack.sim.CloseCalls(fixture.orderID))
		inv = r.getInvoice(ctx, fixture.invoiceID)
		r.expect(inv != nil && inv.PaymentStatus != types.PaymentStatusSucceeded,
			"invoice stays unpaid after a status query (status %v)", invoicePaymentStatus(inv))
	}

	return fixture
}

// ---------------------------------------------------------------------------
// Scenario 2: callback security — foreign signature and field mismatches.
// ---------------------------------------------------------------------------

func (r *smokeRun) scenarioCallbackValidation(ctx context.Context, fixture *paymentFixture) {
	r.section("2. Callback verification: foreign signature, wrong merchant/app/amount/currency/tenant/order")

	if fixture == nil {
		r.fail("callback validation skipped: native fixture was not created")
		return
	}
	orderID := fixture.orderID
	paymentID := fixture.payment.ID
	now := time.Now()

	successPlaintext := wechatsim.TransactionJSON(wechatsim.TransactionParams{
		AppID: r.stack.fixture.AppID, MchID: r.stack.fixture.MerchantID,
		OutTradeNo: orderID, TransactionID: "won't-be-used",
		TradeState: "SUCCESS", TradeType: "NATIVE",
		SuccessTime: now.Format("2006-01-02T15:04:05+08:00"),
		TotalFen:    10000, Currency: "CNY",
	})

	// (a) Signed by a foreign platform key: must fail before any business effect.
	attacker, err := wechatsim.NewFixture()
	if err != nil {
		r.fail("could not create attacker fixture: %v", err)
	} else {
		headers, body, err := attacker.SignedNotification("TRANSACTION.SUCCESS", "transaction", successPlaintext)
		if err != nil {
			r.fail("could not build foreign-signed notification: %v", err)
		} else {
			resp := r.postCallback(ctx, smokeTenantID, smokeEnvID, headers, body)
			r.expectNot2xx(resp, "callback signed by a foreign platform key")
		}
	}

	// (b) Correctly signed variants with a foreign merchant / app / amount / currency / order.
	cases := []struct {
		name      string
		plaintext []byte
	}{
		{"wrong merchant mchid", wechatsim.TransactionJSON(wechatsim.TransactionParams{
			AppID: r.stack.fixture.AppID, MchID: "1900000002",
			OutTradeNo: orderID, TransactionID: "42000000FAKEWRONG01",
			TradeState: "SUCCESS", TradeType: "NATIVE",
			SuccessTime: now.Format("2006-01-02T15:04:05+08:00"),
			TotalFen:    10000, Currency: "CNY",
		})},
		{"wrong appid", wechatsim.TransactionJSON(wechatsim.TransactionParams{
			AppID: "wxfakeotherapp00001", MchID: r.stack.fixture.MerchantID,
			OutTradeNo: orderID, TransactionID: "42000000FAKEWRONG02",
			TradeState: "SUCCESS", TradeType: "NATIVE",
			SuccessTime: now.Format("2006-01-02T15:04:05+08:00"),
			TotalFen:    10000, Currency: "CNY",
		})},
		{"wrong amount", wechatsim.TransactionJSON(wechatsim.TransactionParams{
			AppID: r.stack.fixture.AppID, MchID: r.stack.fixture.MerchantID,
			OutTradeNo: orderID, TransactionID: "42000000FAKEWRONG03",
			TradeState: "SUCCESS", TradeType: "NATIVE",
			SuccessTime: now.Format("2006-01-02T15:04:05+08:00"),
			TotalFen:    9900, Currency: "CNY",
		})},
		{"wrong currency", wechatsim.TransactionJSON(wechatsim.TransactionParams{
			AppID: r.stack.fixture.AppID, MchID: r.stack.fixture.MerchantID,
			OutTradeNo: orderID, TransactionID: "42000000FAKEWRONG04",
			TradeState: "SUCCESS", TradeType: "NATIVE",
			SuccessTime: now.Format("2006-01-02T15:04:05+08:00"),
			TotalFen:    10000, Currency: "USD",
		})},
		{"unknown order", wechatsim.TransactionJSON(wechatsim.TransactionParams{
			AppID: r.stack.fixture.AppID, MchID: r.stack.fixture.MerchantID,
			OutTradeNo: "wx-never-created-by-this-tenant", TransactionID: "42000000FAKEWRONG05",
			TradeState: "SUCCESS", TradeType: "NATIVE",
			SuccessTime: now.Format("2006-01-02T15:04:05+08:00"),
			TotalFen:    10000, Currency: "CNY",
		})},
	}
	for _, tc := range cases {
		headers, body, err := r.stack.fixture.SignedNotification("TRANSACTION.SUCCESS", "transaction", tc.plaintext)
		if err != nil {
			r.fail("could not build %s notification: %v", tc.name, err)
			continue
		}
		resp := r.postCallback(ctx, smokeTenantID, smokeEnvID, headers, body)
		r.expectNot2xx(resp, "callback with "+tc.name)
	}

	// (c) Correctly signed but delivered under a different tenant scope.
	headers, body, err := r.stack.fixture.SignedNotification("TRANSACTION.SUCCESS", "transaction", successPlaintext)
	if err == nil {
		resp := r.postCallback(ctx, "tenant_not_this_one", smokeEnvID, headers, body)
		r.expectNot2xx(resp, "callback routed to a foreign tenant")
	} else {
		r.fail("could not build wrong-tenant notification: %v", err)
	}

	// Nothing above may have settled the payment or the invoice.
	queried := r.getPayment(ctx, paymentID)
	r.expect(queried != nil && queried.PaymentStatus == types.PaymentStatusPending,
		"payment is still PENDING after all rejected callbacks, got %v", paymentStatus(queried))
	inv := r.getInvoice(ctx, fixture.invoiceID)
	r.expect(inv != nil && inv.PaymentStatus != types.PaymentStatusSucceeded,
		"invoice is still unpaid after all rejected callbacks (status %v)", invoicePaymentStatus(inv))

	// (d) The authoritative confirmation: a correctly signed callback settles once.
	r.settleViaCallback(ctx, orderID)

	settled := r.getPayment(ctx, paymentID)
	r.expect(settled != nil && settled.PaymentStatus == types.PaymentStatusSucceeded,
		"payment SUCCEEDED after the verified callback, got %v", paymentStatus(settled))
	if settled != nil {
		r.expect(settled.GatewayPaymentID != nil && *settled.GatewayPaymentID == r.stack.sim.Order(orderID).TransactionID,
			"payment records the provider transaction id %s, got %v", r.stack.sim.Order(orderID).TransactionID, settled.GatewayPaymentID)
	}
	r.expectInvoicePaid(ctx, fixture.invoiceID, "100.00")

	// (e) A redelivered callback must not double-settle.
	dupResp := r.settleRaw(ctx, orderID)
	r.expect(dupResp != nil && dupResp.success(), "duplicate callback is acknowledged (status %d)", statusOf(dupResp))
	r.expectInvoicePaid(ctx, fixture.invoiceID, "100.00")
	r.expectPaymentAmountUnchanged(ctx, paymentID, "100.00")

	// (f) Repeating the status query after settlement also changes nothing.
	r.getPayment(ctx, paymentID)
	r.expectInvoicePaid(ctx, fixture.invoiceID, "100.00")
}

// ---------------------------------------------------------------------------
// Scenario 3: JSAPI parameters and pre-network validation.
// ---------------------------------------------------------------------------

func (r *smokeRun) scenarioJSAPI(ctx context.Context) {
	r.section("3. JSAPI: signed in-browser parameters; invalid requests fail before any network call")

	requestsBefore := len(r.stack.sim.Requests())

	// Missing openid must be rejected before the payment row or network call.
	resp := r.createPaymentRaw(ctx, r.createInvoiceOrFail(ctx, "100.00"), "100.00", map[string]any{
		"payment_type": "jsapi",
	})
	r.expectNot2xx(resp, "jsapi payment without openid")
	// Unknown channel must be rejected too.
	resp = r.createPaymentRaw(ctx, r.createInvoiceOrFail(ctx, "100.00"), "100.00", map[string]any{
		"payment_type": "h5",
	})
	r.expectNot2xx(resp, "jsapi payment with an unsupported channel")
	r.expect(len(r.stack.sim.Requests()) == requestsBefore,
		"no gateway request is sent for rejected channels (%d -> %d)", requestsBefore, len(r.stack.sim.Requests()))

	fixture := r.createPaymentFixtureWithOptions(ctx, "100.00", map[string]any{
		"payment_type": "jsapi",
		"openid":       "oFakeOpenIDForLocalSmoke0001",
	})
	if fixture == nil {
		return
	}

	action := fixture.payment.NextAction
	r.expect(action != nil && action.Type == types.PaymentActionTypeJSAPI,
		"jsapi payment returns a jsapi next action, got %v", actionType(action))
	if action != nil && action.JSAPI != nil {
		params := action.JSAPI
		order := r.stack.sim.Order(fixture.orderID)
		r.expect(params.AppID == r.stack.fixture.AppID, "jsapi appId is the connection app, got %s", params.AppID)
		r.expect(params.SignType == "RSA", "jsapi signType is RSA, got %s", params.SignType)
		r.expect(params.Package == "prepay_id="+prepayID(order), "jsapi package carries the prepay id, got %s", params.Package)
		r.expect(params.TimeStamp != "" && params.NonceStr != "", "jsapi carries timestamp and nonce")
		message := params.AppID + "\n" + params.TimeStamp + "\n" + params.NonceStr + "\n" + params.Package + "\n"
		if err := wechatsim.VerifySHA256WithRSA(message, params.PaySign, r.stack.fixture.MerchantPublicKey()); err != nil {
			r.fail("jsapi paySign does not verify against the merchant key: %v", err)
		} else {
			r.ok("jsapi paySign verifies against the fake merchant public key")
		}
		r.expect(order != nil && order.PayerOpenID == "oFakeOpenIDForLocalSmoke0001",
			"gateway order carries the payer openid, got %v", payerOpenID(order))
	} else {
		r.fail("jsapi payment response has no jsapi parameters")
	}
	r.expectEveryoneSigned()

	// Amount/currency guards: all must fail before a row or a network call.
	guardInvoice := r.createInvoiceOrFail(ctx, "100.00")
	guards := []struct {
		name   string
		amount string
		body   map[string]any
	}{
		{"non-CNY currency", "100.00", map[string]any{"currency": "USD"}},
		{"fractional fen", "100.005", nil},
		{"zero amount", "0", nil},
		{"negative amount", "-1", nil},
		{"amount below outstanding", "50.00", nil},
		{"amount above outstanding", "150.00", nil},
		{"fen overflow", "100000000000000000.00", nil},
	}
	requestCount := len(r.stack.sim.Requests())
	for _, guard := range guards {
		resp := r.createPaymentRawWithBody(ctx, guardInvoice, guard.amount, guard.body)
		r.expectNot2xx(resp, "payment with "+guard.name)
	}
	r.expect(len(r.stack.sim.Requests()) == requestCount,
		"no gateway request is sent for rejected amounts (%d -> %d)", requestCount, len(r.stack.sim.Requests()))

	rows := r.paymentRowsForInvoice(ctx, guardInvoice)
	r.expect(len(rows) == 0, "no payment row exists for rejected requests, found %d", len(rows))
}

// ---------------------------------------------------------------------------
// Scenario 4: 30-minute expiry with query-before-close, both outcomes.
// ---------------------------------------------------------------------------

func (r *smokeRun) scenarioExpiryRace(ctx context.Context) {
	r.section("4. Expiry: query-before-close settles a late payment and only closes confirmed non-payment")

	// (a) Paid-race: the payer paid just before the deadline; a blind close would lose the money.
	paid := r.createPaymentFixture(ctx, "100.00", nativeOptions())
	if paid != nil {
		r.stageExpiry(ctx, paid.payment.ID)
		r.stack.sim.MarkPaid(paid.orderID, time.Now().Add(-30*time.Second))
		reconciled := r.getPayment(ctx, paid.payment.ID)
		r.expect(reconciled != nil && reconciled.PaymentStatus == types.PaymentStatusSucceeded,
			"expired-but-paid order settles to SUCCEEDED on query, got %v", paymentStatus(reconciled))
		r.expect(r.stack.sim.CloseCalls(paid.orderID) == 0,
			"query-before-close never closes a paid order (close calls %d)", r.stack.sim.CloseCalls(paid.orderID))
		r.expectInvoicePaid(ctx, paid.invoiceID, "100.00")
	}

	// (b) Confirmed non-payment: query says NOTPAY past the deadline, then close, then FAILED.
	unpaid := r.createPaymentFixture(ctx, "100.00", nativeOptions())
	if unpaid != nil {
		r.stageExpiry(ctx, unpaid.payment.ID)
		failed := r.getPayment(ctx, unpaid.payment.ID)
		r.expect(failed != nil && failed.PaymentStatus == types.PaymentStatusFailed,
			"expired unpaid order fails after query+close, got %v", paymentStatus(failed))
		if failed != nil && failed.ErrorMessage != nil {
			r.expect(*failed.ErrorMessage != "",
				"failure records a reason (got %q)", *failed.ErrorMessage)
		}
		r.expect(r.stack.sim.CloseCalls(unpaid.orderID) == 1,
			"unpaid expired order is closed exactly once, got %d", r.stack.sim.CloseCalls(unpaid.orderID))
		state := r.stack.sim.Order(unpaid.orderID)
		r.expect(state != nil && state.TradeState == "CLOSED",
			"simulated gateway closed the order, state %v", tradeState(state))
		inv := r.getInvoice(ctx, unpaid.invoiceID)
		r.expect(inv != nil && inv.PaymentStatus != types.PaymentStatusSucceeded,
			"invoice stays unpaid when the order closes (status %v)", invoicePaymentStatus(inv))

		// Re-querying a failed payment must not close or touch anything again.
		r.getPayment(ctx, unpaid.payment.ID)
		r.expect(r.stack.sim.CloseCalls(unpaid.orderID) == 1,
			"a terminal payment is not reconciled again (close calls %d)", r.stack.sim.CloseCalls(unpaid.orderID))
	}
}

// ---------------------------------------------------------------------------
// Scenario 5: refunds — full async refunds settle once via callback and query;
// partial refunds are rejected before anything is persisted or sent.
// ---------------------------------------------------------------------------

func (r *smokeRun) scenarioRefunds(ctx context.Context, settled *paymentFixture) {
	r.section("5. Refunds: full-only, asynchronous acceptance, settle via callback or query")

	if settled == nil {
		r.fail("refund scenario skipped: settled payment fixture missing")
		return
	}

	// (a) Full refund of the settled native payment, settled by REFUND.SUCCESS callback.
	refundA := r.planFullRefund(ctx, settled)
	if refundA != nil {
		r.expect(refundA.RefundStatus == types.RefundStatusProcessing,
			"gateway refund acceptance is PROCESSING, got %s", refundA.RefundStatus)
		r.expect(refundA.PaymentGateway != nil && *refundA.PaymentGateway == string(types.PaymentGatewayTypeWechatPay),
			"refund is bound to wechatpay, got %v", refundA.PaymentGateway)

		outRefundNo := refundOutRefundNo(refundA)
		state := r.stack.sim.Refund(outRefundNo)
		r.expect(state != nil, "gateway saw the refund create for %s", outRefundNo)
		if state != nil {
			r.expect(state.RefundFen == 10000 && state.TotalFen == 10000,
				"gateway refund is for the full original amount (refund %d, total %d)", state.RefundFen, state.TotalFen)
			r.expect(state.Status == "PROCESSING", "gateway accepted the refund asynchronously, status %s", state.Status)
		}

		// The provider's asynchronous outcome arrives as a signed REFUND.SUCCESS callback.
		refundID := ""
		if state != nil {
			refundID = state.RefundID
		}
		plaintext := wechatsim.RefundJSON(wechatsim.RefundParams{
			OutTradeNo:    settled.orderID,
			TransactionID: transactionID(r.stack.sim.Order(settled.orderID)),
			OutRefundNo:   outRefundNo,
			RefundID:      refundID,
			Status:        "SUCCESS",
			SuccessTime:   time.Now().Format("2006-01-02T15:04:05+08:00"),
			RefundFen:     10000,
			TotalFen:      10000,
			Currency:      "CNY",
		})
		headers, body, err := r.stack.fixture.SignedNotification("REFUND.SUCCESS", "refund", plaintext)
		if err != nil {
			r.fail("could not build refund notification: %v", err)
		} else {
			resp := r.postCallback(ctx, smokeTenantID, smokeEnvID, headers, body)
			r.expect(resp != nil && resp.success(), "verified refund callback is acknowledged (status %d)", statusOf(resp))
		}

		after := r.getRefund(ctx, refundA.ID)
		r.expect(after != nil && after.RefundStatus == types.RefundStatusSucceeded,
			"refund SUCCEEDED after the verified callback, got %s", refundStatus(after))
		if after != nil {
			r.expect(after.SettledAmount.Equal(decimal.RequireFromString("100.00")),
				"settled amount is the full 100.00, got %s", after.SettledAmount)
		}

		// Redelivery must not re-settle.
		if err == nil {
			r.postCallback(ctx, smokeTenantID, smokeEnvID, headers, body)
		}
		afterDup := r.getRefund(ctx, refundA.ID)
		r.expect(afterDup != nil && afterDup.SettledAmount.Equal(decimal.RequireFromString("100.00")),
			"duplicate refund callback does not double the settled amount, got %s", settledAmount(afterDup))
	}

	// (b) Full refund settled by the signed status query instead of a callback.
	second := r.createPaymentFixture(ctx, "100.00", nativeOptions())
	if second != nil {
		r.settleViaCallback(ctx, second.orderID)
		refundB := r.planFullRefund(ctx, second)
		if refundB != nil {
			outRefundNo := refundOutRefundNo(refundB)
			r.stack.sim.MarkRefundSuccess(outRefundNo)
			reconciled := r.getRefund(ctx, refundB.ID)
			r.expect(reconciled != nil && reconciled.RefundStatus == types.RefundStatusSucceeded,
				"refund SUCCEEDED through the signed query path, got %s", refundStatus(reconciled))
			if reconciled != nil {
				r.expect(reconciled.SettledAmount.Equal(decimal.RequireFromString("100.00")),
					"query-settled refund carries the full amount, got %s", reconciled.SettledAmount)
			}
		}
	}

	// (c) Partial refund against a settled wechatpay payment: rejected before persistence/network.
	partialInvoice := r.createPaymentFixture(ctx, "100.00", nativeOptions())
	if partialInvoice != nil {
		r.settleViaCallback(ctx, partialInvoice.orderID)

		gatewayCallsBefore := len(r.stack.sim.RefundRequests())
		refundsBefore := len(r.listRefundsForInvoice(ctx, partialInvoice.invoiceID))

		cn := r.createCreditNote(ctx, partialInvoice.invoiceID, partialInvoice.lineItemID, "50.00")
		if cn != nil {
			resp := r.finalizeCreditNote(ctx, cn.ID)
			r.expectNot2xx(resp, "partial wechatpay refund finalize")
			r.expect(len(r.stack.sim.RefundRequests()) == gatewayCallsBefore,
				"partial refund never reaches the gateway (%d -> %d calls)", gatewayCallsBefore, len(r.stack.sim.RefundRequests()))
			r.expect(len(r.listRefundsForInvoice(ctx, partialInvoice.invoiceID)) == refundsBefore,
				"partial refund leaves no refund rows behind")
		}
	}
}

// ---------------------------------------------------------------------------
// Scenario 6: existing outbound events reach an isolated receiver.
// ---------------------------------------------------------------------------

func (r *smokeRun) scenarioOutboundEvents(ctx context.Context) {
	r.section("6. Outbound events: existing event stream reaches an isolated receiver")

	failures := r.stack.webhooks.Failures()
	r.expect(len(failures) == 0, "no event failed to render/deliver, got %v", failures)

	captured := r.stack.webhooks.Events()
	required := []types.WebhookEventName{
		types.WebhookEventPaymentCreated,
		types.WebhookEventPaymentPending,
		types.WebhookEventPaymentUpdated,
		types.WebhookEventInvoiceUpdatePayment,
		types.WebhookEventRefundCreated,
		types.WebhookEventRefundSucceeded,
	}
	for _, name := range required {
		r.expect(r.stack.webhooks.countEvents(name) > 0, "existing event %s was emitted", name)
	}

	delivered := r.stack.receiver.Delivered()
	r.expect(len(delivered) == len(captured),
		"every captured event reached the isolated receiver (%d delivered vs %d captured)", len(delivered), len(captured))

	superseded := r.stack.webhooks.deliveredPayloadsFor(types.WebhookEventPaymentUpdated)
	foundSucceeded := false
	for _, payload := range superseded {
		if payment, ok := payload["payment"].(map[string]any); ok && payment["payment_status"] == "SUCCEEDED" {
			foundSucceeded = true
		}
	}
	r.expect(foundSucceeded, "an invoice.payment-style payment.updated payload reports SUCCEEDED")

	invoicePayloads := r.stack.webhooks.deliveredPayloadsFor(types.WebhookEventInvoiceUpdatePayment)
	foundPaidInvoice := false
	for _, payload := range invoicePayloads {
		if inv, ok := payload["invoice"].(map[string]any); ok && inv["id"] != nil {
			raw, _ := json.Marshal(payload)
			if strings.Contains(string(raw), "SUCCEEDED") {
				foundPaidInvoice = true
			}
		}
	}
	r.expect(foundPaidInvoice, "an invoice.update.payment payload reports the paid state")

	refundPayloads := r.stack.webhooks.deliveredPayloadsFor(types.WebhookEventRefundSucceeded)
	foundRefund := false
	for _, payload := range refundPayloads {
		raw, _ := json.Marshal(payload)
		if strings.Contains(string(raw), `"refund_status":"SUCCEEDED"`) && strings.Contains(string(raw), `"settled_amount":"100"`) {
			foundRefund = true
		}
	}
	r.expect(foundRefund, "a refund.succeeded payload carries the settled 100.00 amount")
}

// ---------------------------------------------------------------------------
// Scenario 7: the connected provider is visible on the integration config
// endpoint with every sync direction off.
// ---------------------------------------------------------------------------

func (r *smokeRun) scenarioIntegrationConfig(ctx context.Context) {
	r.section("7. Integration config: connected WeChat Pay provider with no sync directions")

	resp, err := r.stack.client.private(ctx, http.MethodGet, "/v1/integrations/config", nil)
	if err != nil {
		r.fail("integrations/config request failed: %v", err)
		return
	}
	if !resp.success() {
		r.fail("integrations/config returned %d: %s", resp.Status, resp.text())
		return
	}
	var config dto.IntegrationConfigResponse
	if err := resp.decode(&config); err != nil {
		r.fail("decode integrations/config: %v", err)
		return
	}

	var entry *dto.IntegrationConfigEntry
	for i := range config.Integrations {
		if config.Integrations[i].Provider == types.SecretProviderWechatPay {
			entry = &config.Integrations[i]
			break
		}
	}
	r.expect(entry != nil, "the connected wechatpay provider is visible in GET /v1/integrations/config")
	if entry == nil {
		return
	}

	r.expect(entry.BaseConfig != nil, "wechatpay entry carries a base_config (previously omitted when nil)")
	if ok, reason := syncDirectionsAllOff(entry.BaseConfig); ok {
		r.ok("every base_config sync direction is false")
	} else {
		r.fail("base_config advertises sync: %s", reason)
	}

	r.expect(entry.CurrentConfig != nil, "wechatpay entry carries a current_config")
	// current_config reflects the stored connection config: an absent direction
	// is optional-and-disabled by schema semantics, so the contract is "no
	// enabled inbound/outbound direction", not "every field present".
	if ok, reason := noEnabledSyncDirections(entry.CurrentConfig); ok {
		r.ok("current_config enables no sync direction (absent fields count as disabled)")
	} else {
		r.fail("current_config advertises sync: %s", reason)
	}
}

// syncDirectionsAllOff reports whether every entity sync direction in the config
// is present and disabled — the capability answer for a manual-only provider.
func syncDirectionsAllOff(sc *types.SyncConfig) (bool, string) {
	if sc == nil {
		return false, "config is nil"
	}
	entities := []struct {
		name   string
		config *types.EntitySyncConfig
	}{
		{"plan", sc.Plan},
		{"subscription", sc.Subscription},
		{"invoice", sc.Invoice},
		{"customer", sc.Customer},
		{"payment", sc.Payment},
		{"price", sc.Price},
		{"deal", sc.Deal},
		{"quote", sc.Quote},
	}
	for _, entity := range entities {
		if entity.config == nil {
			return false, entity.name + " direction is missing"
		}
		if entity.config.Inbound || entity.config.Outbound {
			return false, entity.name + " direction is enabled"
		}
	}
	return true, ""
}

// noEnabledSyncDirections reports whether the config enables nothing. Unlike
// the base_config check, a missing entity direction is allowed here: the stored
// connection config is optional per entity, and an absent direction is
// disabled by schema semantics rather than an advertised capability.
func noEnabledSyncDirections(sc *types.SyncConfig) (bool, string) {
	if sc == nil {
		return true, ""
	}
	entities := []struct {
		name   string
		config *types.EntitySyncConfig
	}{
		{"plan", sc.Plan},
		{"subscription", sc.Subscription},
		{"invoice", sc.Invoice},
		{"customer", sc.Customer},
		{"payment", sc.Payment},
		{"price", sc.Price},
		{"deal", sc.Deal},
		{"quote", sc.Quote},
	}
	for _, entity := range entities {
		if entity.config == nil {
			continue
		}
		if entity.config.Inbound || entity.config.Outbound {
			return false, entity.name + " direction is enabled"
		}
	}
	return true, ""
}

// ---------------------------------------------------------------------------
// Shared fixture/action helpers.
// ---------------------------------------------------------------------------

func nativeOptions() map[string]any {
	return map[string]any{"payment_type": "native"}
}

func (r *smokeRun) createPaymentFixture(ctx context.Context, amount string, options map[string]any) *paymentFixture {
	return r.createPaymentFixtureWithOptions(ctx, amount, options)
}

func (r *smokeRun) createPaymentFixtureWithOptions(ctx context.Context, amount string, options map[string]any) *paymentFixture {
	invoiceID, lineItemID, ok := r.createInvoice(ctx, amount)
	if !ok {
		return nil
	}
	payment := r.createWechatPayPayment(ctx, invoiceID, amount, options)
	if payment == nil {
		return nil
	}
	if payment.GatewayTrackingID == nil || *payment.GatewayTrackingID == "" {
		r.fail("payment response carries no gateway_tracking_id (order handle)")
		return nil
	}
	orderID := *payment.GatewayTrackingID
	r.expect(orderID == wechatpay.MerchantOrderID(payment.ID),
		"order handle %s is the deterministic derivation of the payment id", orderID)
	return &paymentFixture{invoiceID: invoiceID, lineItemID: lineItemID, payment: payment, orderID: orderID}
}

func (r *smokeRun) createInvoice(ctx context.Context, amount string) (string, string, bool) {
	body := map[string]any{
		"customer_id":    "cust_smoke_wechatpay",
		"currency":       "CNY",
		"amount_due":     amount,
		"total":          amount,
		"subtotal":       amount,
		"invoice_type":   "ONE_OFF",
		"invoice_status": "FINALIZED",
		"payment_status": "PENDING",
		"description":    "wechatpay smoke invoice",
		// A pre-set PDF url keeps the outbound invoice payload builder off the
		// object-storage path, which this offline harness does not provide.
		"invoice_pdf_url": "https://invoice.smoke.example.com/invoice.pdf",
		"line_items": []map[string]any{{
			"display_name": "Smoke item",
			"amount":       amount,
			"quantity":     "1",
			"price_type":   "FIXED",
		}},
	}
	resp, err := r.stack.client.private(ctx, http.MethodPost, "/v1/invoices", body)
	if err != nil {
		r.fail("create invoice request failed: %v", err)
		return "", "", false
	}
	if !resp.success() {
		r.fail("create invoice returned %d: %s", resp.Status, resp.text())
		return "", "", false
	}
	var inv dto.InvoiceResponse
	if err := resp.decode(&inv); err != nil {
		r.fail("decode invoice response: %v", err)
		return "", "", false
	}
	if inv.PaymentStatus == types.PaymentStatusSucceeded {
		r.fail("newly created invoice is unexpectedly paid")
		return "", "", false
	}
	lineItemID := ""
	if len(inv.LineItems) > 0 && inv.LineItems[0] != nil {
		lineItemID = inv.LineItems[0].ID
	}
	if inv.ID == "" || lineItemID == "" {
		r.fail("invoice response missing id or line item id")
		return "", "", false
	}
	return inv.ID, lineItemID, true
}

func (r *smokeRun) createInvoiceOrFail(ctx context.Context, amount string) string {
	id, _, ok := r.createInvoice(ctx, amount)
	if !ok {
		return ""
	}
	return id
}

func (r *smokeRun) createWechatPayPayment(ctx context.Context, invoiceID, amount string, options map[string]any) *dto.PaymentResponse {
	body := map[string]any{
		"destination_type":    "INVOICE",
		"destination_id":      invoiceID,
		"payment_method_type": "PAYMENT_LINK",
		"payment_gateway":     "wechatpay",
		"amount":              amount,
		"currency":            "CNY",
		"process_payment":     true,
		"gateway_options":     map[string]any{"wechatpay": options},
	}
	resp, err := r.stack.client.private(ctx, http.MethodPost, "/v1/payments", body)
	if err != nil {
		r.fail("create payment request failed: %v", err)
		return nil
	}
	if !resp.success() {
		r.fail("create payment returned %d: %s", resp.Status, resp.text())
		return nil
	}
	var payment dto.PaymentResponse
	if err := resp.decode(&payment); err != nil {
		r.fail("decode payment response: %v", err)
		return nil
	}
	if payment.NextAction == nil {
		r.fail("payment response carries no next_action")
		return nil
	}
	if payment.NextAction.Type != types.PaymentActionTypeQRCode && payment.NextAction.Type != types.PaymentActionTypeJSAPI {
		r.fail("payment next_action has unexpected type %q", payment.NextAction.Type)
	}
	if payment.ExpiresAt == nil {
		r.fail("payment response carries no expires_at")
	} else {
		r.expect(payment.ExpiresAt.After(time.Now().Add(29*time.Minute)) && payment.ExpiresAt.Before(time.Now().Add(31*time.Minute)),
			"expires_at is the fixed ~30 minute deadline, got %s", payment.ExpiresAt.Format(time.RFC3339))
	}
	return &payment
}

// createPaymentRaw posts an arbitrary wechatpay payment body and returns the raw response.
func (r *smokeRun) createPaymentRaw(ctx context.Context, invoiceID, amount string, options map[string]any) *apiResponse {
	body := map[string]any{
		"destination_type":    "INVOICE",
		"destination_id":      invoiceID,
		"payment_method_type": "PAYMENT_LINK",
		"payment_gateway":     "wechatpay",
		"amount":              amount,
		"currency":            "CNY",
		"process_payment":     true,
		"gateway_options":     map[string]any{"wechatpay": options},
	}
	resp, err := r.stack.client.private(ctx, http.MethodPost, "/v1/payments", body)
	if err != nil {
		r.fail("create payment request failed: %v", err)
		return nil
	}
	return resp
}

func (r *smokeRun) createPaymentRawWithBody(ctx context.Context, invoiceID, amount string, overrides map[string]any) *apiResponse {
	body := map[string]any{
		"destination_type":    "INVOICE",
		"destination_id":      invoiceID,
		"payment_method_type": "PAYMENT_LINK",
		"payment_gateway":     "wechatpay",
		"amount":              amount,
		"currency":            "CNY",
		"process_payment":     true,
		"gateway_options":     map[string]any{"wechatpay": nativeOptions()},
	}
	for key, value := range overrides {
		body[key] = value
	}
	resp, err := r.stack.client.private(ctx, http.MethodPost, "/v1/payments", body)
	if err != nil {
		r.fail("create payment request failed: %v", err)
		return nil
	}
	return resp
}

func (r *smokeRun) getPayment(ctx context.Context, paymentID string) *dto.PaymentResponse {
	resp, err := r.stack.client.private(ctx, http.MethodGet, "/v1/payments/"+paymentID, nil)
	if err != nil {
		r.fail("get payment request failed: %v", err)
		return nil
	}
	if !resp.success() {
		r.fail("get payment returned %d: %s", resp.Status, resp.text())
		return nil
	}
	var payment dto.PaymentResponse
	if err := resp.decode(&payment); err != nil {
		r.fail("decode payment: %v", err)
		return nil
	}
	return &payment
}

func (r *smokeRun) getInvoice(ctx context.Context, invoiceID string) *dto.InvoiceResponse {
	resp, err := r.stack.client.private(ctx, http.MethodGet, "/v1/invoices/"+invoiceID, nil)
	if err != nil {
		r.fail("get invoice request failed: %v", err)
		return nil
	}
	if !resp.success() {
		r.fail("get invoice returned %d: %s", resp.Status, resp.text())
		return nil
	}
	var inv dto.InvoiceResponse
	if err := resp.decode(&inv); err != nil {
		r.fail("decode invoice: %v", err)
		return nil
	}
	return &inv
}

func (r *smokeRun) postCallback(ctx context.Context, tenantID, envID string, headers http.Header, body []byte) *apiResponse {
	path := "/v1/webhooks/wechatpay/" + tenantID + "/" + envID
	resp, err := r.stack.client.raw(ctx, http.MethodPost, path, body, headers)
	if err != nil {
		r.fail("post callback failed: %v", err)
		return nil
	}
	return resp
}

// settleViaCallback delivers a correctly signed TRANSACTION.SUCCESS for an order.
func (r *smokeRun) settleViaCallback(ctx context.Context, orderID string) {
	resp := r.settleRaw(ctx, orderID)
	r.expect(resp != nil && resp.success(), "verified success callback acknowledged for %s (status %d)", orderID, statusOf(resp))
}

func (r *smokeRun) settleRaw(ctx context.Context, orderID string) *apiResponse {
	order := r.stack.sim.Order(orderID)
	if order != nil && order.TradeState != "SUCCESS" {
		// Deliver the payer's payment first, the way the provider settles it
		// before notifying; settlement callbacks are only ever issued for paid orders.
		r.stack.sim.MarkPaid(orderID, time.Now())
		order = r.stack.sim.Order(orderID)
	}
	plaintext := wechatsim.TransactionJSON(wechatsim.TransactionParams{
		AppID:         r.stack.fixture.AppID,
		MchID:         r.stack.fixture.MerchantID,
		OutTradeNo:    orderID,
		TransactionID: transactionID(order),
		TradeState:    "SUCCESS",
		TradeType:     "NATIVE",
		SuccessTime:   time.Now().Format("2006-01-02T15:04:05+08:00"),
		TotalFen:      10000,
		Currency:      "CNY",
	})
	headers, body, err := r.stack.fixture.SignedNotification("TRANSACTION.SUCCESS", "transaction", plaintext)
	if err != nil {
		r.fail("could not build success notification: %v", err)
		return nil
	}
	return r.postCallback(ctx, smokeTenantID, smokeEnvID, headers, body)
}

// stageExpiry rewrites the persisted deadline of an attempt into the past,
// which is how the harness time-travels past the fixed 30-minute validity.
func (r *smokeRun) stageExpiry(ctx context.Context, paymentID string) {
	storeCtx := r.stack.requestContext()
	p, err := r.stack.stores.payment.Get(storeCtx, paymentID)
	if err != nil {
		r.fail("stage expiry: load payment: %v", err)
		return
	}
	if p.GatewayMetadata == nil {
		p.GatewayMetadata = types.Metadata{}
	}
	p.GatewayMetadata[dto.GatewayMetadataWechatPayExpiresAt] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if err := r.stack.stores.payment.Update(storeCtx, p); err != nil {
		r.fail("stage expiry: persist payment: %v", err)
	}
}

func (r *smokeRun) createCreditNote(ctx context.Context, invoiceID, lineItemID, amount string) *dto.CreditNoteResponse {
	body := map[string]any{
		"invoice_id":          invoiceID,
		"reason":              "ORDER_CHANGE",
		"process_credit_note": false,
		"line_items": []map[string]any{{
			"invoice_line_item_id": lineItemID,
			"display_name":         "wechatpay smoke refund",
			"amount":               amount,
		}},
	}
	resp, err := r.stack.client.private(ctx, http.MethodPost, "/v1/creditnotes", body)
	if err != nil {
		r.fail("create credit note request failed: %v", err)
		return nil
	}
	if !resp.success() {
		r.fail("create credit note returned %d: %s", resp.Status, resp.text())
		return nil
	}
	var cn dto.CreditNoteResponse
	if err := resp.decode(&cn); err != nil {
		r.fail("decode credit note: %v", err)
		return nil
	}
	if cn.CreditNote == nil || cn.ID == "" {
		r.fail("credit note response missing id")
		return nil
	}
	return &cn
}

func (r *smokeRun) finalizeCreditNote(ctx context.Context, creditNoteID string) *apiResponse {
	resp, err := r.stack.client.private(ctx, http.MethodPost, "/v1/creditnotes/"+creditNoteID+"/finalize",
		map[string]any{"refund_target": "BACK_TO_SOURCE"})
	if err != nil {
		r.fail("finalize credit note request failed: %v", err)
		return nil
	}
	return resp
}

// planFullRefund drives the credit-note path that creates and dispatches one
// full gateway refund, returning the canonical row.
func (r *smokeRun) planFullRefund(ctx context.Context, payment *paymentFixture) *dto.RefundResponse {
	cn := r.createCreditNote(ctx, payment.invoiceID, payment.lineItemID, "100.00")
	if cn == nil {
		return nil
	}
	resp := r.finalizeCreditNote(ctx, cn.ID)
	if resp == nil || !resp.success() {
		r.fail("full refund finalize returned %d: %s", statusOf(resp), bodyOf(resp))
		return nil
	}
	rows := r.listRefundsForInvoice(ctx, payment.invoiceID)
	if len(rows) != 1 || rows[0] == nil || rows[0].Refund == nil {
		r.fail("expected exactly one refund row for the invoice, got %d", len(rows))
		return nil
	}
	return rows[0]
}

func (r *smokeRun) listRefundsForInvoice(ctx context.Context, invoiceID string) []*dto.RefundResponse {
	resp, err := r.stack.client.private(ctx, http.MethodGet, "/v1/refunds?invoice_ids="+invoiceID, nil)
	if err != nil {
		r.fail("list refunds request failed: %v", err)
		return nil
	}
	if !resp.success() {
		r.fail("list refunds returned %d: %s", resp.Status, resp.text())
		return nil
	}
	var list dto.ListRefundsResponse
	if err := resp.decode(&list); err != nil {
		r.fail("decode refunds: %v", err)
		return nil
	}
	return list.Items
}

func (r *smokeRun) getRefund(ctx context.Context, refundID string) *dto.RefundResponse {
	resp, err := r.stack.client.private(ctx, http.MethodGet, "/v1/refunds/"+refundID, nil)
	if err != nil {
		r.fail("get refund request failed: %v", err)
		return nil
	}
	if !resp.success() {
		r.fail("get refund returned %d: %s", resp.Status, resp.text())
		return nil
	}
	var row dto.RefundResponse
	if err := resp.decode(&row); err != nil {
		r.fail("decode refund: %v", err)
		return nil
	}
	return &row
}

func (r *smokeRun) paymentRowsForInvoice(ctx context.Context, invoiceID string) []string {
	storeCtx := r.stack.requestContext()
	filter := types.NewNoLimitPaymentFilter()
	filter.DestinationID = &invoiceID
	rows, err := r.stack.stores.payment.List(storeCtx, filter)
	if err != nil {
		r.fail("list payment rows: %v", err)
		return nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func (r *smokeRun) expectInvoicePaid(ctx context.Context, invoiceID, amount string) {
	inv := r.getInvoice(ctx, invoiceID)
	if inv == nil {
		return
	}
	r.expect(inv.PaymentStatus == types.PaymentStatusSucceeded,
		"invoice %s is PAID after authoritative confirmation, got %v", invoiceID, inv.PaymentStatus)
	want := decimal.RequireFromString(amount)
	r.expect(inv.AmountPaid.Equal(want),
		"invoice %s amount_paid is exactly %s (no double settlement), got %s", invoiceID, amount, inv.AmountPaid)
}

func (r *smokeRun) expectPaymentAmountUnchanged(ctx context.Context, paymentID, amount string) {
	payment := r.getPayment(ctx, paymentID)
	if payment == nil {
		return
	}
	r.expect(payment.Amount.Equal(decimal.RequireFromString(amount)),
		"payment %s amount remains %s, got %s", paymentID, amount, payment.Amount)
	r.expect(payment.PaymentStatus == types.PaymentStatusSucceeded,
		"payment %s remains SUCCEEDED, got %v", paymentID, payment.PaymentStatus)
}

// ---------------------------------------------------------------------------
// Assertion plumbing.
// ---------------------------------------------------------------------------

func (r *smokeRun) section(title string) {
	fmt.Printf("\n== %s\n", title)
}

func (r *smokeRun) ok(format string, args ...any) {
	fmt.Printf("  [ok] %s\n", fmt.Sprintf(format, args...))
}

func (r *smokeRun) expect(cond bool, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if cond {
		r.ok("%s", message)
		return
	}
	fmt.Printf("  [FAIL] %s\n", message)
	r.failures = append(r.failures, message)
}

func (r *smokeRun) fail(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	fmt.Printf("  [FAIL] %s\n", message)
	r.failures = append(r.failures, message)
}

func (r *smokeRun) expectNot2xx(resp *apiResponse, what string) {
	r.expect(resp != nil && !resp.success(), "%s is rejected with a non-2xx status (got %d %s)", what, statusOf(resp), bodyOf(resp))
}

// expectEveryRequestSigned asserts the simulated gateway verified the merchant
// signature on every request it received so far.
func (r *smokeRun) expectEveryRequestSigned(what string) {
	requests := r.stack.sim.Requests()
	unsigned := 0
	firstErr := ""
	for _, req := range requests {
		if !req.SignatureVerified {
			unsigned++
			if firstErr == "" {
				firstErr = fmt.Sprintf("%s %s: %s", req.Method, req.Path, req.VerifyError)
			}
		}
	}
	r.expect(len(requests) > 0 && unsigned == 0,
		"every %s request carried a valid merchant signature (%d requests, %d unsigned%s)",
		what, len(requests), unsigned, verificationReasonSuffix(firstErr))
}

func (r *smokeRun) expectEveryoneSigned() {
	requests := r.stack.sim.Requests()
	unsigned := 0
	firstErr := ""
	for _, req := range requests {
		if !req.SignatureVerified {
			unsigned++
			if firstErr == "" {
				firstErr = fmt.Sprintf("%s %s: %s", req.Method, req.Path, req.VerifyError)
			}
		}
	}
	r.expect(unsigned == 0, "no unsigned request ever reached the simulated gateway (%d unsigned of %d%s)",
		unsigned, len(requests), verificationReasonSuffix(firstErr))
}

func verificationReasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return "; first failure: " + reason
}

func statusOf(resp *apiResponse) int {
	if resp == nil {
		return 0
	}
	return resp.Status
}

func bodyOf(resp *apiResponse) string {
	if resp == nil {
		return ""
	}
	return truncateForError(resp.Body)
}

func paymentStatus(p *dto.PaymentResponse) any {
	if p == nil {
		return "<nil>"
	}
	return p.PaymentStatus
}

func invoicePaymentStatus(inv *dto.InvoiceResponse) any {
	if inv == nil {
		return "<nil>"
	}
	return inv.PaymentStatus
}

func refundStatus(row *dto.RefundResponse) any {
	if row == nil || row.Refund == nil {
		return "<nil>"
	}
	return row.RefundStatus
}

func settledAmount(row *dto.RefundResponse) any {
	if row == nil || row.Refund == nil {
		return "<nil>"
	}
	return row.SettledAmount
}

func actionType(action *types.PaymentAction) any {
	if action == nil {
		return "<nil>"
	}
	return action.Type
}

func tradeState(order *wechatsim.OrderState) any {
	if order == nil {
		return "<nil>"
	}
	return order.TradeState
}

func payerOpenID(order *wechatsim.OrderState) any {
	if order == nil {
		return "<nil>"
	}
	return order.PayerOpenID
}

func prepayID(order *wechatsim.OrderState) string {
	if order == nil {
		return ""
	}
	return order.PrepayID
}

func transactionID(order *wechatsim.OrderState) string {
	if order == nil {
		return ""
	}
	return order.TransactionID
}

// refundOutRefundNo derives the merchant refund number the production adapter
// used for a refund row.
func refundOutRefundNo(row *dto.RefundResponse) string {
	if row == nil || row.Refund == nil {
		return ""
	}
	key := row.IdempotencyKey
	if row.GatewayIdempotencyToken != nil && *row.GatewayIdempotencyToken != "" {
		key = *row.GatewayIdempotencyToken
	}
	return wechatpay.MerchantRefundNo(key)
}
