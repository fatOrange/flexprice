package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/flexprice/flexprice/internal/domain/invoice"
	"github.com/flexprice/flexprice/internal/domain/payment"
	"github.com/flexprice/flexprice/internal/domain/refund"
	"github.com/flexprice/flexprice/internal/integration/wechatpay"
	"github.com/flexprice/flexprice/internal/interfaces"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	wechatsim "github.com/flexprice/flexprice/cmd/wechatpay-smoke/wechatsim"
)

// settleOrderViaCallback drives the payer to settle the simulated order and
// delivers the signed TRANSACTION.SUCCESS callback over the real HTTP route.
func (s *wechatPayTestStack) settleOrderViaCallback(t *testing.T, router *gin.Engine, orderID string, amountFen int64) *httptest.ResponseRecorder {
	t.Helper()
	s.payOrder(orderID)
	headers, body := s.signedTransaction(orderID, amountFen, "", "")
	w := s.postCallback(router, wpTenantID, wpEnvID, headers, body)
	require.Equal(t, http.StatusOK, w.Code, "callback body: %s", w.Body.String())
	return w
}

func TestWechatPayCallbackSettlesOnceAndRejectsForgedNotifications(t *testing.T) {
	stack := newWechatPayTestStack(t)
	stack.seedFinalizedInvoice(t, "inv_wp_callback", "100.00")
	row := stack.seedOrderAndPayment(t, "pay_wp_callback", "inv_wp_callback", "100.00")
	orderID := *row.GatewayTrackingID
	router := stack.callbackRouter()

	// A notification signed by a different platform key is refused outright.
	attacker, err := wechatsim.NewFixture()
	require.NoError(t, err)
	plaintext := wechatsim.TransactionJSON(wechatsim.TransactionParams{
		AppID: stack.fixture.AppID, MchID: stack.fixture.MerchantID,
		OutTradeNo: orderID, TransactionID: "42000000FAKEFORGED",
		TradeState: "SUCCESS", TradeType: "NATIVE",
		SuccessTime: time.Now().Format("2006-01-02T15:04:05+08:00"),
		TotalFen:    10000, Currency: "CNY",
	})
	forgedHeaders, forgedBody, err := attacker.SignedNotification("TRANSACTION.SUCCESS", "transaction", plaintext)
	require.NoError(t, err)
	require.GreaterOrEqual(t,
		stack.postCallback(router, wpTenantID, wpEnvID, forgedHeaders, forgedBody).Code, 400,
		"a foreign platform signature must never be acknowledged")

	// Correctly signed but inconsistent fields are refused per-field.
	stack.payOrder(orderID)
	mismatched := []struct {
		name      string
		amountFen int64
		mchID     string
		appID     string
	}{
		{"wrong merchant", 10000, "1900000002", ""},
		{"wrong app", 10000, "", "wxfakeotherapp00001"},
		{"wrong amount", 9900, "", ""},
	}
	for _, tc := range mismatched {
		headers, body := stack.signedTransaction(orderID, tc.amountFen, tc.mchID, tc.appID)
		w := stack.postCallback(router, wpTenantID, wpEnvID, headers, body)
		require.GreaterOrEqual(t, w.Code, 400, "%s must be rejected", tc.name)
	}

	// A signed notification for an order this tenant never created is refused.
	unknownHeaders, unknownBody := stack.signedTransaction("wx-never-created-by-this-tenant", 10000, "", "")
	require.GreaterOrEqual(t,
		stack.postCallback(router, wpTenantID, wpEnvID, unknownHeaders, unknownBody).Code, 400)

	// A callback delivered under a foreign tenant scope is refused.
	require.GreaterOrEqual(t,
		stack.postCallback(router, "tenant_not_this_one", wpEnvID, unknownHeaders, unknownBody).Code, 400)

	// Nothing above may have moved the payment or the invoice.
	before := stack.mustGetPayment(t, "pay_wp_callback")
	require.Equal(t, types.PaymentStatusPending, before.PaymentStatus)
	unpaid := stack.mustGetInvoice(t, "inv_wp_callback")
	require.NotEqual(t, types.PaymentStatusSucceeded, unpaid.PaymentStatus)

	// A verified callback now settles payment and invoice exactly once.
	stack.settleOrderViaCallback(t, router, orderID, 10000)

	settled := stack.mustGetPayment(t, "pay_wp_callback")
	require.Equal(t, types.PaymentStatusSucceeded, settled.PaymentStatus)
	require.NotNil(t, settled.GatewayPaymentID)
	require.Equal(t, orderTransactionID(stack.sim.Order(orderID)), *settled.GatewayPaymentID)
	paid := stack.mustGetInvoice(t, "inv_wp_callback")
	require.Equal(t, types.PaymentStatusSucceeded, paid.PaymentStatus)
	require.True(t, paid.AmountPaid.Equal(decimal.RequireFromString("100.00")), "amount_paid=%s", paid.AmountPaid)

	// Redelivery is absorbed without double settlement.
	stack.settleOrderViaCallback(t, router, orderID, 10000)
	redelivered := stack.mustGetPayment(t, "pay_wp_callback")
	require.Equal(t, types.PaymentStatusSucceeded, redelivered.PaymentStatus)
	stillPaid := stack.mustGetInvoice(t, "inv_wp_callback")
	require.True(t, stillPaid.AmountPaid.Equal(decimal.RequireFromString("100.00")), "amount_paid=%s", stillPaid.AmountPaid)
}

func TestWechatPayQueryPaymentResolvesExpiryRace(t *testing.T) {
	stack := newWechatPayTestStack(t)

	// (a) The payer paid just before the deadline: the query must settle, not close.
	stack.seedFinalizedInvoice(t, "inv_wp_race_paid", "100.00")
	paidRow := stack.seedOrderAndPayment(t, "pay_wp_race_paid", "inv_wp_race_paid", "100.00")
	paidOrderID := *paidRow.GatewayTrackingID
	stack.stageExpiry(t, paidRow.ID)
	stack.payOrder(paidOrderID)

	paid, err := stack.wechatService.QueryPayment(stack.ctx, paidRow.ID)
	require.NoError(t, err)
	require.Equal(t, types.PaymentStatusSucceeded, paid.PaymentStatus)
	require.Equal(t, 0, stack.sim.CloseCalls(paidOrderID), "a paid order must never be closed")
	require.Equal(t, types.PaymentStatusSucceeded, stack.mustGetInvoice(t, "inv_wp_race_paid").PaymentStatus)

	// (b) Confirmed non-payment past the deadline: query, close, then FAILED.
	stack.seedFinalizedInvoice(t, "inv_wp_race_unpaid", "100.00")
	unpaidRow := stack.seedOrderAndPayment(t, "pay_wp_race_unpaid", "inv_wp_race_unpaid", "100.00")
	unpaidOrderID := *unpaidRow.GatewayTrackingID
	stack.stageExpiry(t, unpaidRow.ID)

	expired, err := stack.wechatService.QueryPayment(stack.ctx, unpaidRow.ID)
	require.NoError(t, err)
	require.Equal(t, types.PaymentStatusFailed, expired.PaymentStatus)
	require.NotNil(t, expired.ErrorMessage)
	require.NotEmpty(t, *expired.ErrorMessage, "a failed expiry records why")
	require.Equal(t, 1, stack.sim.CloseCalls(unpaidOrderID))
	require.Equal(t, "CLOSED", stack.sim.Order(unpaidOrderID).TradeState)
	require.NotEqual(t, types.PaymentStatusSucceeded, stack.mustGetInvoice(t, "inv_wp_race_unpaid").PaymentStatus)

	// A terminal payment is not reconciled again.
	_, err = stack.wechatService.QueryPayment(stack.ctx, unpaidRow.ID)
	require.NoError(t, err)
	require.Equal(t, 1, stack.sim.CloseCalls(unpaidOrderID))
}

func TestWechatPayRefundAdapterFullOnlyAndCallbackSettlement(t *testing.T) {
	stack := newWechatPayTestStack(t)
	stack.seedFinalizedInvoice(t, "inv_wp_refund", "100.00")
	row := stack.seedOrderAndPayment(t, "pay_wp_refund", "inv_wp_refund", "100.00")
	orderID := *row.GatewayTrackingID
	router := stack.callbackRouter()
	stack.settleOrderViaCallback(t, router, orderID, 10000)

	settled := stack.mustGetPayment(t, "pay_wp_refund")
	require.NotNil(t, settled.GatewayPaymentID)
	transactionID := *settled.GatewayPaymentID

	adapter := wechatpay.NewRefundAdapter(stack.gateway)
	idempotencyKey := "wp-verification-refund-0001"

	full, err := adapter.RefundPayment(stack.ctx, interfaces.RefundProviderRequest{
		GatewayPaymentID: transactionID,
		Amount:           decimal.RequireFromString("100.00"),
		Currency:         "CNY",
		IdempotencyKey:   idempotencyKey,
	})
	require.NoError(t, err)
	require.Equal(t, types.RefundStatusProcessing, full.Status, "WeChat Pay accepts refunds asynchronously")
	require.NotEmpty(t, full.GatewayRefundID)

	outRefundNo := wechatpay.MerchantRefundNo(idempotencyKey)
	state := stack.sim.Refund(outRefundNo)
	require.NotNil(t, state, "the gateway must have seen the full refund create")
	require.Equal(t, int64(10000), state.RefundFen)
	require.Equal(t, int64(10000), state.TotalFen)
	require.Equal(t, "PROCESSING", state.Status)
	createCalls := countRefundCreates(stack)

	// A partial refund never reaches the gateway.
	_, err = adapter.RefundPayment(stack.ctx, interfaces.RefundProviderRequest{
		GatewayPaymentID: transactionID,
		Amount:           decimal.RequireFromString("50.00"),
		Currency:         "CNY",
		IdempotencyKey:   "wp-verification-refund-0002",
	})
	require.Error(t, err, "partial refunds are not supported")
	require.Equal(t, createCalls, countRefundCreates(stack), "no second refund may be created")

	// The canonical refund row is settled by the verified callback, exactly once.
	refundRow := &refund.Refund{
		ID:                      "ref_wp_verification",
		PaymentID:               new("pay_wp_refund"),
		InvoiceID:               "inv_wp_refund",
		PaymentGateway:          new(string(types.PaymentGatewayTypeWechatPay)),
		GatewayRefundID:         new(state.RefundID),
		Amount:                  decimal.RequireFromString("100.00"),
		Currency:                "cny",
		RefundStatus:            types.RefundStatusProcessing,
		RefundReason:            types.RefundReasonRequestedByCustomer,
		RefundDestination:       types.RefundDestinationGateway,
		IdempotencyKey:          idempotencyKey,
		GatewayIdempotencyToken: new(idempotencyKey),
		EnvironmentID:           wpEnvID,
		BaseModel:               types.BaseModel{TenantID: wpTenantID, Status: types.StatusPublished},
	}
	require.NoError(t, stack.refundRepo.Create(stack.ctx, refundRow))

	headers, body := stack.signedRefund(orderID, outRefundNo, state.RefundID, 10000)
	w := stack.postCallback(router, wpTenantID, wpEnvID, headers, body)
	require.Equal(t, http.StatusOK, w.Code, "refund callback body: %s", w.Body.String())

	settledRefund, err := stack.refundRepo.Get(stack.ctx, refundRow.ID)
	require.NoError(t, err)
	require.Equal(t, types.RefundStatusSucceeded, settledRefund.RefundStatus)
	require.True(t, settledRefund.SettledAmount.Equal(decimal.RequireFromString("100.00")), "settled=%s", settledRefund.SettledAmount)

	// Redelivery cannot double-settle.
	w = stack.postCallback(router, wpTenantID, wpEnvID, headers, body)
	require.Equal(t, http.StatusOK, w.Code)
	afterDuplicate, err := stack.refundRepo.Get(stack.ctx, refundRow.ID)
	require.NoError(t, err)
	require.True(t, afterDuplicate.SettledAmount.Equal(decimal.RequireFromString("100.00")), "settled=%s", afterDuplicate.SettledAmount)
}

func TestWechatPayCheckoutAdapterNativeAndJSAPI(t *testing.T) {
	stack := newWechatPayTestStack(t)
	adapter := wechatpay.NewCheckoutAdapter(stack.gateway)
	expiresAt := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)

	native, err := adapter.CreatePaymentLink(stack.ctx, interfaces.CheckoutProviderRequest{
		InvoiceID:  "inv_wp_adapter",
		CustomerID: wpCustomer,
		PaymentID:  "pay_wp_adapter_native",
		Amount:     decimal.RequireFromString("100.00"),
		Currency:   "CNY",
		ExpiresAt:  &expiresAt,
	})
	require.NoError(t, err)
	require.Equal(t, types.PaymentActionTypeQRCode, native.NextAction.Type)
	require.True(t, len(native.NextAction.QRContent) > 0, "native action must carry the QR payload")

	nativeOrder := stack.sim.Order(wechatpay.MerchantOrderID("pay_wp_adapter_native"))
	require.NotNil(t, nativeOrder)
	require.Equal(t, int64(10000), nativeOrder.AmountFen)
	require.Equal(t, "CNY", nativeOrder.Currency)
	require.True(t, nativeOrder.TimeExpire.Equal(expiresAt), "time_expire must be the persisted attempt expiry")
	require.True(t,
		strings.HasSuffix(nativeOrder.NotifyURL, "/v1/webhooks/wechatpay/"+wpTenantID+"/"+wpEnvID),
		"notify_url must route to this tenant/environment, got %s", nativeOrder.NotifyURL)

	jsapi, err := adapter.CreatePaymentLink(stack.ctx, interfaces.CheckoutProviderRequest{
		InvoiceID:  "inv_wp_adapter",
		CustomerID: wpCustomer,
		PaymentID:  "pay_wp_adapter_jsapi",
		Amount:     decimal.RequireFromString("100.00"),
		Currency:   "CNY",
		ExpiresAt:  &expiresAt,
		WechatPayOptions: &types.WechatPayOptions{
			PaymentType: types.WechatPayPaymentTypeJSAPI,
			OpenID:      "oFakeOpenIDForAdapterTest01",
		},
	})
	require.NoError(t, err)
	require.Equal(t, types.PaymentActionTypeJSAPI, jsapi.NextAction.Type)
	require.NotNil(t, jsapi.NextAction.JSAPI)
	params := jsapi.NextAction.JSAPI
	require.Equal(t, stack.fixture.AppID, params.AppID)
	require.Equal(t, "RSA", params.SignType)
	require.Equal(t, "prepay_id="+stack.sim.Order(wechatpay.MerchantOrderID("pay_wp_adapter_jsapi")).PrepayID, params.Package)

	message := params.AppID + "\n" + params.TimeStamp + "\n" + params.NonceStr + "\n" + params.Package + "\n"
	require.NoError(t,
		wechatsim.VerifySHA256WithRSA(message, params.PaySign, stack.fixture.MerchantPublicKey()),
		"jsapi paySign must verify against the merchant key")

	// JSAPI without an openid is invalid before any network call.
	_, err = adapter.CreatePaymentLink(stack.ctx, interfaces.CheckoutProviderRequest{
		InvoiceID:  "inv_wp_adapter",
		CustomerID: wpCustomer,
		PaymentID:  "pay_wp_adapter_invalid",
		Amount:     decimal.RequireFromString("100.00"),
		Currency:   "CNY",
		ExpiresAt:  &expiresAt,
		WechatPayOptions: &types.WechatPayOptions{
			PaymentType: types.WechatPayPaymentTypeJSAPI,
		},
	})
	require.Error(t, err)

	// Every request the simulator saw carried a valid merchant signature.
	for _, recorded := range stack.sim.Requests() {
		require.True(t, recorded.SignatureVerified, "request %s %s was not signed", recorded.Method, recorded.Path)
	}
}

func TestWechatPayCallbackRejectsMalformedResourceNonce(t *testing.T) {
	stack := newWechatPayTestStack(t)
	stack.seedFinalizedInvoice(t, "inv_wp_nonce", "100.00")
	row := stack.seedOrderAndPayment(t, "pay_wp_nonce", "inv_wp_nonce", "100.00")
	router := stack.callbackRouter()

	// A correctly signed envelope whose resource nonce is not the 12 bytes GCM
	// requires: the provider must refuse it before any cipher call, panic-free.
	envelope := map[string]any{
		"id":            "EV-wp-malformed-nonce",
		"create_time":   time.Now().Format("2006-01-02T15:04:05+08:00"),
		"event_type":    "TRANSACTION.SUCCESS",
		"resource_type": "encrypt-resource",
		"resource": map[string]string{
			"algorithm":       "AEAD_AES_256_GCM",
			"ciphertext":      "ZmFrZS1jaXBoZXJ0ZXh0",
			"nonce":           "KpMKDqcmpnieBcXb", // 16 chars: the historical base64-of-12-bytes shape
			"associated_data": "fake-associated-data",
			"original_type":   "transaction",
		},
		"summary": "malformed nonce",
	}
	body, err := json.Marshal(envelope)
	require.NoError(t, err)
	headers, err := stack.fixture.SignNotificationBody(body)
	require.NoError(t, err)

	w := stack.postCallback(router, wpTenantID, wpEnvID, headers, body)
	require.GreaterOrEqual(t, w.Code, 400, "a malformed resource nonce must be rejected")
	require.Less(t, w.Code, 500, "it is a validation refusal, not a server error")

	// The rejection must not echo raw SDK error material or the request headers.
	require.NotContains(t, w.Body.String(), "Wechatpay-Signature")
	require.NotContains(t, w.Body.String(), "ZmFrZS1jaXBoZXJ0ZXh0")

	// And nothing may have moved.
	still := stack.mustGetPayment(t, row.ID)
	require.Equal(t, types.PaymentStatusPending, still.PaymentStatus)
	require.NotEqual(t, types.PaymentStatusSucceeded, stack.mustGetInvoice(t, "inv_wp_nonce").PaymentStatus)
}

func (s *wechatPayTestStack) mustGetPayment(t *testing.T, paymentID string) *payment.Payment {
	t.Helper()
	row, err := s.paymentRepo.Get(s.ctx, paymentID)
	require.NoError(t, err)
	return row
}

func (s *wechatPayTestStack) mustGetInvoice(t *testing.T, invoiceID string) *invoice.Invoice {
	t.Helper()
	row, err := s.invoiceRepo.Get(s.ctx, invoiceID)
	require.NoError(t, err)
	return row
}

// countRefundCreates counts POST /v3/refund/domestic/refunds requests the
// simulator received.
func countRefundCreates(stack *wechatPayTestStack) int {
	creates := 0
	for _, recorded := range stack.sim.Requests() {
		if recorded.Method == http.MethodPost && recorded.Path == "/v3/refund/domestic/refunds" {
			creates++
		}
	}
	return creates
}
