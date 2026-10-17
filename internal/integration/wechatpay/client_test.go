package wechatpay

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/interfaces"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTransactionID = "4200001234202610121234567890"

// Mock provider paths shared by the close/query/refund negative tests.
const (
	testQueryPath  = "/v3/pay/transactions/out-trade-no/pay_01TESTPAYMENT"
	testClosePath  = "/v3/pay/transactions/out-trade-no/pay_01TESTPAYMENT/close"
	testRefundPath = "/v3/refund/domestic/refunds/refund_01TESTREFUND"
)

// testCheckoutRequest builds a checkout attempt whose expiry is a fixed offset
// from the moment the test runs. A hardcoded wall-clock date turns into a past
// deadline as soon as it passes, which would fail the attempt-expiry guard
// instead of exercising the provider call under test.
func testCheckoutRequest() interfaces.CheckoutProviderRequest {
	expires := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	return interfaces.CheckoutProviderRequest{
		InvoiceID:  "inv_01TESTINVOICE",
		PaymentID:  "pay_01TESTPAYMENT",
		CustomerID: "cus_01TESTCUSTOMER",
		Amount:     decimal.RequireFromString("12.34"),
		Currency:   "CNY",
		ExpiresAt:  &expires,
	}
}

func successTransactionBody() string {
	return `{
		"appid": "` + testAppID + `",
		"mchid": "` + testMerchantID + `",
		"out_trade_no": "pay_01TESTPAYMENT",
		"transaction_id": "` + testTransactionID + `",
		"trade_state": "SUCCESS",
		"success_time": "2026-10-12T10:00:00+08:00",
		"amount": {"total": 1234, "currency": "CNY"}
	}`
}

func refundBodyWithStatus(status string) string {
	return `{
		"refund_id": "5000000038202610121234567890",
		"out_refund_no": "refund_01TESTREFUND",
		"transaction_id": "` + testTransactionID + `",
		"out_trade_no": "pay_01TESTPAYMENT",
		"status": "` + status + `",
		"success_time": "2026-10-12T10:05:00+08:00",
		"amount": {"total": 1234, "refund": 1234, "currency": "CNY"}
	}`
}

func processingRefundBody() string {
	return refundBodyWithStatus(RefundStatusProcessing)
}

func newTestGateway(t *testing.T) (*signedMockGateway, Gateway, *testKeys) {
	t.Helper()
	keys := generateTestKeys(t)
	mock := newSignedMockGateway(t, keys)
	gateway := mock.client(t, &fakeConnectionRepo{conn: mock.connection(t, keys)})
	return mock, gateway, keys
}

func TestCreatePaymentNative(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodPost, "/v3/pay/transactions/native", func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: `{"code_url":"weixin://wxpay/bizpayurl?pr=mocknative"}`}
	})

	req := testCheckoutRequest()
	resp, err := gateway.CreatePayment(testContext(), req, types.WechatPayOptions{})
	require.NoError(t, err)

	assert.Equal(t, "pay_01TESTPAYMENT", resp.ProviderSessionID)
	assert.Equal(t, types.PaymentActionTypeQRCode, resp.NextAction.Type)
	assert.Equal(t, "weixin://wxpay/bizpayurl?pr=mocknative", resp.NextAction.QRContent)
	assert.Nil(t, resp.NextAction.JSAPI)
	require.NotNil(t, resp.ExpiresAt)
	assert.True(t, req.ExpiresAt.Equal(*resp.ExpiresAt), "response expiry must be the persisted attempt expiry")

	recorded := mock.recorded()
	require.Len(t, recorded, 1)
	var body struct {
		Appid       string `json:"appid"`
		Mchid       string `json:"mchid"`
		Description string `json:"description"`
		OutTradeNo  string `json:"out_trade_no"`
		TimeExpire  string `json:"time_expire"`
		NotifyUrl   string `json:"notify_url"`
		Amount      struct {
			Total    int64  `json:"total"`
			Currency string `json:"currency"`
		} `json:"amount"`
	}
	require.NoError(t, json.Unmarshal(recorded[0].Body, &body))
	assert.Equal(t, testAppID, body.Appid)
	assert.Equal(t, testMerchantID, body.Mchid)
	assert.Equal(t, "Invoice inv_01TESTINVOICE", body.Description)
	assert.Equal(t, "pay_01TESTPAYMENT", body.OutTradeNo)
	assert.Equal(t, req.ExpiresAt.In(chinaStandardTime).Format(time.RFC3339), body.TimeExpire, "time_expire must be the persisted expiry in +08:00")
	assert.Equal(t, testNotifyURLBase+"/tenant_test/env_test", body.NotifyUrl)
	assert.Equal(t, int64(1234), body.Amount.Total)
	assert.Equal(t, "CNY", body.Amount.Currency)
}

func TestCreatePaymentJSAPI(t *testing.T) {
	mock, gateway, keys := newTestGateway(t)
	mock.handle(http.MethodPost, "/v3/pay/transactions/jsapi", func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: `{"prepay_id":"wx-mock-prepay-id"}`}
	})

	req := testCheckoutRequest()
	resp, err := gateway.CreatePayment(testContext(), req, types.WechatPayOptions{
		PaymentType: types.WechatPayPaymentTypeJSAPI,
		OpenID:      "oTestOpenID",
	})
	require.NoError(t, err)

	assert.Equal(t, types.PaymentActionTypeJSAPI, resp.NextAction.Type)
	require.NotNil(t, resp.NextAction.JSAPI)
	params := resp.NextAction.JSAPI
	assert.Equal(t, testAppID, params.AppID)
	assert.Equal(t, "wx-mock-prepay-id", resp.ProviderMetadata["prepay_id"])
	assert.Equal(t, "prepay_id=wx-mock-prepay-id", params.Package)
	assert.Equal(t, "RSA", params.SignType)
	assert.NotEmpty(t, params.NonceStr)
	epoch, err := strconv.ParseInt(params.TimeStamp, 10, 64)
	require.NoError(t, err, "timeStamp must be unix seconds as a string")
	assert.WithinDuration(t, time.Now(), time.Unix(epoch, 0), time.Minute)

	// The paySign must verify against the merchant public key over exactly
	// appId\ntimeStamp\nnonceStr\npackage\n.
	message := params.AppID + "\n" + params.TimeStamp + "\n" + params.NonceStr + "\n" + params.Package + "\n"
	signature, err := base64.StdEncoding.DecodeString(params.PaySign)
	require.NoError(t, err)
	hashed := sha256.Sum256([]byte(message))
	require.NoError(t, rsa.VerifyPKCS1v15(&keys.merchantPrivate.PublicKey, crypto.SHA256, hashed[:], signature))

	recorded := mock.recorded()
	require.Len(t, recorded, 1)
	var body struct {
		Payer struct {
			Openid string `json:"openid"`
		} `json:"payer"`
	}
	require.NoError(t, json.Unmarshal(recorded[0].Body, &body))
	assert.Equal(t, "oTestOpenID", body.Payer.Openid)
}

func TestCreatePaymentRejectsInvalidInputBeforeNetwork(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	expired := time.Now().Add(-time.Minute)
	cases := []struct {
		name   string
		mutate func(*interfaces.CheckoutProviderRequest)
		option types.WechatPayOptions
	}{
		{name: "non cny", mutate: func(r *interfaces.CheckoutProviderRequest) { r.Currency = "USD" }},
		{name: "fractional fen", mutate: func(r *interfaces.CheckoutProviderRequest) { r.Amount = decimal.RequireFromString("12.345") }},
		{name: "zero amount", mutate: func(r *interfaces.CheckoutProviderRequest) { r.Amount = decimal.Zero }},
		{name: "negative amount", mutate: func(r *interfaces.CheckoutProviderRequest) { r.Amount = decimal.RequireFromString("-1.00") }},
		{name: "overflow amount", mutate: func(r *interfaces.CheckoutProviderRequest) {
			r.Amount = decimal.RequireFromString("92233720368547758.08")
		}},
		{name: "expired attempt", mutate: func(r *interfaces.CheckoutProviderRequest) { r.ExpiresAt = &expired }},
		{name: "missing payment id", mutate: func(r *interfaces.CheckoutProviderRequest) { r.PaymentID = "" }},
		{name: "unknown channel", option: types.WechatPayOptions{PaymentType: "h5"}},
		{name: "jsapi without openid", option: types.WechatPayOptions{PaymentType: types.WechatPayPaymentTypeJSAPI}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := testCheckoutRequest()
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			_, err := gateway.CreatePayment(testContext(), req, tc.option)
			require.Error(t, err)
			assert.True(t, ierr.IsValidation(err), "expected validation error, got %v", err)
		})
	}
	assert.Empty(t, mock.recorded(), "validation failures must not reach the provider")
}

func TestGetTransaction(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodGet, "/v3/pay/transactions/out-trade-no/pay_01TESTPAYMENT", func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: successTransactionBody()}
	})

	tx, err := gateway.GetTransaction(testContext(), "pay_01TESTPAYMENT")
	require.NoError(t, err)
	assert.Equal(t, "pay_01TESTPAYMENT", tx.OutTradeNo)
	assert.Equal(t, testTransactionID, tx.TransactionID)
	assert.Equal(t, TradeStateSuccess, tx.TradeState)
	assert.Equal(t, int64(1234), tx.Amount.Total)
	assert.Equal(t, "CNY", tx.Amount.Currency)
	assert.Equal(t, "2026-10-12T10:00:00+08:00", tx.SuccessTime)

	recorded := mock.recorded()
	require.Len(t, recorded, 1)
	assert.Equal(t, testMerchantID, recorded[0].Query.Get("mchid"))
}

func TestGetTransactionNotFound(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodGet, "/v3/pay/transactions/out-trade-no/pay_01TESTPAYMENT", func(req *recordedRequest) mockResponse {
		return jsonResponse(http.StatusNotFound, map[string]string{
			"code": "ORDER_NOT_EXIST", "message": "order not exist",
		})
	})

	_, err := gateway.GetTransaction(testContext(), "pay_01TESTPAYMENT")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOrderNotFound)
	assert.True(t, ierr.IsNotFound(err))
}

func TestGetTransactionByID(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodGet, "/v3/pay/transactions/id/"+testTransactionID, func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: successTransactionBody()}
	})

	tx, err := gateway.GetTransactionByID(testContext(), testTransactionID)
	require.NoError(t, err)
	assert.Equal(t, testTransactionID, tx.TransactionID)
}

func TestCloseOrder(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodPost, "/v3/pay/transactions/out-trade-no/pay_01TESTPAYMENT/close", func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusNoContent, Body: ""}
	})

	require.NoError(t, gateway.CloseOrder(testContext(), "pay_01TESTPAYMENT"))
	recorded := mock.recorded()
	require.Len(t, recorded, 1)
	assert.JSONEq(t, `{"mchid":"`+testMerchantID+`"}`, string(recorded[0].Body))
}

func TestCloseOrderPaidRace(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodPost, "/v3/pay/transactions/out-trade-no/pay_01TESTPAYMENT/close", func(req *recordedRequest) mockResponse {
		return jsonResponse(http.StatusBadRequest, map[string]string{
			"code": "ORDERPAID", "message": "order is paid",
		})
	})

	err := gateway.CloseOrder(testContext(), "pay_01TESTPAYMENT")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOrderPaid)
}

func TestCloseOrderAlreadyClosedIsSuccess(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodPost, "/v3/pay/transactions/out-trade-no/pay_01TESTPAYMENT/close", func(req *recordedRequest) mockResponse {
		return jsonResponse(http.StatusBadRequest, map[string]string{
			"code": "ORDER_CLOSED", "message": "order is closed",
		})
	})

	require.NoError(t, gateway.CloseOrder(testContext(), "pay_01TESTPAYMENT"))
}

func TestCloseOrderNotFound(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodPost, "/v3/pay/transactions/out-trade-no/pay_01TESTPAYMENT/close", func(req *recordedRequest) mockResponse {
		return jsonResponse(http.StatusNotFound, map[string]string{
			"code": "ORDER_NOT_EXIST", "message": "order not exist",
		})
	})

	err := gateway.CloseOrder(testContext(), "pay_01TESTPAYMENT")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOrderNotFound)
}

func TestGetRefund(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodGet, "/v3/refund/domestic/refunds/refund_01TESTREFUND", func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: processingRefundBody()}
	})

	refund, err := gateway.GetRefund(testContext(), "refund_01TESTREFUND")
	require.NoError(t, err)
	assert.Equal(t, "refund_01TESTREFUND", refund.OutRefundNo)
	assert.Equal(t, "5000000038202610121234567890", refund.RefundID)
	assert.Equal(t, RefundStatusProcessing, refund.RefundStatus)
	assert.Equal(t, int64(1234), refund.Amount.Total)
	assert.Equal(t, int64(1234), refund.Amount.Refund)
}

func TestGetRefundNotFound(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodGet, "/v3/refund/domestic/refunds/refund_01TESTREFUND", func(req *recordedRequest) mockResponse {
		return jsonResponse(http.StatusNotFound, map[string]string{
			"code": "RESOURCE_NOT_EXISTS", "message": "refund not exist",
		})
	})

	_, err := gateway.GetRefund(testContext(), "refund_01TESTREFUND")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRefundNotFound)
}

func testRefundRequest(amount string) interfaces.RefundProviderRequest {
	return interfaces.RefundProviderRequest{
		GatewayPaymentID: testTransactionID,
		Amount:           decimal.RequireFromString(amount),
		Currency:         "CNY",
		IdempotencyKey:   "refund_01TESTREFUND",
	}
}

func TestRefundAdapterFullRefundAcceptedAsProcessing(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodGet, "/v3/pay/transactions/id/"+testTransactionID, func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: successTransactionBody()}
	})
	mock.handle(http.MethodGet, "/v3/refund/domestic/refunds/refund_01TESTREFUND", func(req *recordedRequest) mockResponse {
		return jsonResponse(http.StatusNotFound, map[string]string{"code": "RESOURCE_NOT_EXISTS"})
	})
	mock.handle(http.MethodPost, "/v3/refund/domestic/refunds", func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: processingRefundBody()}
	})

	adapter := NewRefundAdapter(gateway)
	resp, err := adapter.RefundPayment(testContext(), testRefundRequest("12.34"))
	require.NoError(t, err)
	assert.Equal(t, types.RefundStatusProcessing, resp.Status, "provider acceptance is PROCESSING, not settled")
	assert.True(t, resp.SettledAmount.IsZero())
	assert.Equal(t, "5000000038202610121234567890", resp.GatewayRefundID)

	recorded := mock.recorded()
	require.Len(t, recorded, 3)
	var createBody struct {
		TransactionId string `json:"transaction_id"`
		OutRefundNo   string `json:"out_refund_no"`
		NotifyUrl     string `json:"notify_url"`
		Amount        struct {
			Total    int64  `json:"total"`
			Refund   int64  `json:"refund"`
			Currency string `json:"currency"`
		} `json:"amount"`
	}
	require.NoError(t, json.Unmarshal(recorded[2].Body, &createBody))
	assert.Equal(t, testTransactionID, createBody.TransactionId)
	assert.Equal(t, "refund_01TESTREFUND", createBody.OutRefundNo)
	assert.Equal(t, testNotifyURLBase+"/tenant_test/env_test", createBody.NotifyUrl)
	assert.Equal(t, int64(1234), createBody.Amount.Total)
	assert.Equal(t, int64(1234), createBody.Amount.Refund)
	assert.Equal(t, "CNY", createBody.Amount.Currency)
}

func TestRefundAdapterRejectsPartialRefund(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodGet, "/v3/pay/transactions/id/"+testTransactionID, func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: successTransactionBody()}
	})

	adapter := NewRefundAdapter(gateway)
	_, err := adapter.RefundPayment(testContext(), testRefundRequest("10.00"))
	require.Error(t, err)
	assert.True(t, ierr.IsValidation(err), "expected validation error, got %v", err)
	assert.Len(t, mock.recorded(), 1, "a partial refund must not reach the refund endpoint")
}

func TestRefundAdapterRejectsUnsettledTransaction(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodGet, "/v3/pay/transactions/id/"+testTransactionID, func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: `{
			"appid": "` + testAppID + `",
			"mchid": "` + testMerchantID + `",
			"out_trade_no": "pay_01TESTPAYMENT",
			"transaction_id": "` + testTransactionID + `",
			"trade_state": "NOTPAY",
			"amount": {"total": 1234, "currency": "CNY"}
		}`}
	})

	adapter := NewRefundAdapter(gateway)
	_, err := adapter.RefundPayment(testContext(), testRefundRequest("12.34"))
	require.Error(t, err)
	assert.True(t, ierr.IsValidation(err), "expected validation error, got %v", err)
	assert.Len(t, mock.recorded(), 1)
}

func TestRefundAdapterReturnsExistingRefundWithoutCreating(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodGet, "/v3/pay/transactions/id/"+testTransactionID, func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: successTransactionBody()}
	})
	mock.handle(http.MethodGet, "/v3/refund/domestic/refunds/refund_01TESTREFUND", func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: processingRefundBody()}
	})

	adapter := NewRefundAdapter(gateway)
	resp, err := adapter.RefundPayment(testContext(), testRefundRequest("12.34"))
	require.NoError(t, err)
	assert.Equal(t, types.RefundStatusProcessing, resp.Status)
	assert.Equal(t, "5000000038202610121234567890", resp.GatewayRefundID)
	assert.Len(t, mock.recorded(), 2, "an existing refund must not be created again")
}

func TestRefundAdapterSettlesInlineSuccess(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodGet, "/v3/pay/transactions/id/"+testTransactionID, func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: successTransactionBody()}
	})
	mock.handle(http.MethodGet, "/v3/refund/domestic/refunds/refund_01TESTREFUND", func(req *recordedRequest) mockResponse {
		return jsonResponse(http.StatusNotFound, map[string]string{"code": "RESOURCE_NOT_EXISTS"})
	})
	mock.handle(http.MethodPost, "/v3/refund/domestic/refunds", func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: `{
			"refund_id": "5000000038202610121234567890",
			"out_refund_no": "refund_01TESTREFUND",
			"transaction_id": "` + testTransactionID + `",
			"out_trade_no": "pay_01TESTPAYMENT",
			"status": "SUCCESS",
			"success_time": "2026-10-12T10:05:00+08:00",
			"amount": {"total": 1234, "refund": 1234, "currency": "CNY"}
		}`}
	})

	adapter := NewRefundAdapter(gateway)
	resp, err := adapter.RefundPayment(testContext(), testRefundRequest("12.34"))
	require.NoError(t, err)
	assert.Equal(t, types.RefundStatusSucceeded, resp.Status)
	assert.True(t, resp.SettledAmount.Equal(decimal.RequireFromString("12.34")))
}

func TestRefundAdapterRejectsForeignGatewayImplementation(t *testing.T) {
	adapter := NewRefundAdapter(&fakeGateway{})
	_, err := adapter.RefundPayment(testContext(), testRefundRequest("12.34"))
	require.Error(t, err)
	assert.True(t, ierr.IsNotImplemented(err), "expected not implemented, got %v", err)
}

func TestProviderErrorsAreNotSentinelLeaks(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodPost, "/v3/pay/transactions/native", func(req *recordedRequest) mockResponse {
		return jsonResponse(http.StatusBadRequest, map[string]string{
			"code": "PARAM_ERROR", "message": "invalid description",
		})
	})

	_, err := gateway.CreatePayment(testContext(), testCheckoutRequest(), types.WechatPayOptions{})
	require.Error(t, err)
	assert.True(t, ierr.IsHTTPClient(err), "expected provider error, got %v", err)
	assert.False(t, errors.Is(err, ErrOrderNotFound))
}

func TestCreatePaymentAcceptsTheLargestRepresentableAmount(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodPost, "/v3/pay/transactions/native", func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: `{"code_url":"weixin://wxpay/bizpayurl?pr=mocknative"}`}
	})

	req := testCheckoutRequest()
	req.Amount = decimal.RequireFromString("92233720368547758.07")
	_, err := gateway.CreatePayment(testContext(), req, types.WechatPayOptions{})
	require.NoError(t, err)

	recorded := mock.recorded()
	require.Len(t, recorded, 1)
	var body struct {
		Amount struct {
			Total int64 `json:"total"`
		} `json:"amount"`
	}
	require.NoError(t, json.Unmarshal(recorded[0].Body, &body))
	assert.Equal(t, int64(9223372036854775807), body.Amount.Total)
}

func TestCreatePaymentDefaultsExpiryWhenCallerHasNone(t *testing.T) {
	mock, gateway, _ := newTestGateway(t)
	mock.handle(http.MethodPost, "/v3/pay/transactions/native", func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: `{"code_url":"weixin://wxpay/bizpayurl?pr=mocknative"}`}
	})

	req := testCheckoutRequest()
	req.ExpiresAt = nil
	before := time.Now().UTC()
	resp, err := gateway.CreatePayment(testContext(), req, types.WechatPayOptions{})
	require.NoError(t, err)

	recorded := mock.recorded()
	require.Len(t, recorded, 1)
	var body struct {
		TimeExpire string `json:"time_expire"`
	}
	require.NoError(t, json.Unmarshal(recorded[0].Body, &body))
	wireExpiry, err := time.Parse(time.RFC3339, body.TimeExpire)
	require.NoError(t, err)
	assert.WithinDuration(t, before.Add(30*time.Minute), wireExpiry, time.Minute)
	require.NotNil(t, resp.ExpiresAt)
	assert.True(t, resp.ExpiresAt.Equal(wireExpiry), "response expiry must match the wire deadline")
}

func TestMerchantPrivateKeyAcceptedInPKCS1Form(t *testing.T) {
	keys := generateTestKeys(t)
	mock := newSignedMockGateway(t, keys)
	conn := mock.connection(t, keys)
	conn.EncryptedSecretData.WechatPay.MerchantPrivateKey = "enc:" + keys.merchantPrivateKeyPKCS1PEM(t)
	mock.handle(http.MethodGet, "/v3/pay/transactions/out-trade-no/pay_01TESTPAYMENT", func(req *recordedRequest) mockResponse {
		return mockResponse{Status: http.StatusOK, Body: successTransactionBody()}
	})
	gateway := mock.client(t, &fakeConnectionRepo{conn: conn})

	tx, err := gateway.GetTransaction(testContext(), "pay_01TESTPAYMENT")
	require.NoError(t, err)
	assert.Equal(t, testTransactionID, tx.TransactionID)
}

func TestRefundAdapterMapsUnsettledProviderStatuses(t *testing.T) {
	cases := []struct {
		status string
		want   types.RefundStatus
	}{
		{status: RefundStatusAbnormal, want: types.RefundStatusFailed},
		{status: RefundStatusClosed, want: types.RefundStatusCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			mock, gateway, _ := newTestGateway(t)
			mock.handle(http.MethodGet, "/v3/pay/transactions/id/"+testTransactionID, func(req *recordedRequest) mockResponse {
				return mockResponse{Status: http.StatusOK, Body: successTransactionBody()}
			})
			mock.handle(http.MethodGet, "/v3/refund/domestic/refunds/refund_01TESTREFUND", func(req *recordedRequest) mockResponse {
				return mockResponse{Status: http.StatusOK, Body: refundBodyWithStatus(tc.status)}
			})

			adapter := NewRefundAdapter(gateway)
			resp, err := adapter.RefundPayment(testContext(), testRefundRequest("12.34"))
			require.NoError(t, err)
			assert.Equal(t, tc.want, resp.Status)
			assert.True(t, resp.SettledAmount.IsZero(), "an unsettled refund reports no settled amount")
		})
	}
}

// TestUnverifiedProviderErrorsCannotDriveState pins the rule that an error
// body is untrusted until the platform signature over it verifies. The SDK
// short-circuits non-2xx responses before its own response validation, so a
// forged 404 ORDER_NOT_EXIST / 400 ORDERPAID / 400 ORDER_CLOSED must never
// surface as a state-affecting sentinel or as close success.
func TestUnverifiedProviderErrorsCannotDriveState(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		path        string
		status      int
		code        string
		tamper      func(*mockResponse)
		action      func(gateway Gateway) error
		notSentinel error
	}{
		{
			name: "missing signature order not found", method: http.MethodGet, path: testQueryPath,
			status: http.StatusNotFound, code: "ORDER_NOT_EXIST",
			tamper: func(r *mockResponse) { r.NoSign = true },
			action: func(gateway Gateway) error {
				_, err := gateway.GetTransaction(testContext(), "pay_01TESTPAYMENT")
				return err
			},
			notSentinel: ErrOrderNotFound,
		},
		{
			name: "forged signature order not found", method: http.MethodGet, path: testQueryPath,
			status: http.StatusNotFound, code: "ORDER_NOT_EXIST",
			tamper: func(r *mockResponse) { r.BadSignature = true },
			action: func(gateway Gateway) error {
				_, err := gateway.GetTransaction(testContext(), "pay_01TESTPAYMENT")
				return err
			},
			notSentinel: ErrOrderNotFound,
		},
		{
			name: "missing signature refund not found", method: http.MethodGet, path: testRefundPath,
			status: http.StatusNotFound, code: "RESOURCE_NOT_EXISTS",
			tamper: func(r *mockResponse) { r.NoSign = true },
			action: func(gateway Gateway) error {
				_, err := gateway.GetRefund(testContext(), "refund_01TESTREFUND")
				return err
			},
			notSentinel: ErrRefundNotFound,
		},
		{
			name: "missing signature order paid", method: http.MethodPost, path: testClosePath,
			status: http.StatusBadRequest, code: "ORDERPAID",
			tamper:      func(r *mockResponse) { r.NoSign = true },
			action:      func(gateway Gateway) error { return gateway.CloseOrder(testContext(), "pay_01TESTPAYMENT") },
			notSentinel: ErrOrderPaid,
		},
		{
			name: "forged signature order paid", method: http.MethodPost, path: testClosePath,
			status: http.StatusBadRequest, code: "ORDERPAID",
			tamper:      func(r *mockResponse) { r.BadSignature = true },
			action:      func(gateway Gateway) error { return gateway.CloseOrder(testContext(), "pay_01TESTPAYMENT") },
			notSentinel: ErrOrderPaid,
		},
		{
			name: "missing signature order closed is not close success", method: http.MethodPost, path: testClosePath,
			status: http.StatusBadRequest, code: "ORDER_CLOSED",
			tamper: func(r *mockResponse) { r.NoSign = true },
			action: func(gateway Gateway) error { return gateway.CloseOrder(testContext(), "pay_01TESTPAYMENT") },
		},
		{
			name: "forged signature order closed is not close success", method: http.MethodPost, path: testClosePath,
			status: http.StatusBadRequest, code: "ORDER_CLOSED",
			tamper: func(r *mockResponse) { r.BadSignature = true },
			action: func(gateway Gateway) error { return gateway.CloseOrder(testContext(), "pay_01TESTPAYMENT") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, gateway, _ := newTestGateway(t)
			mock.handle(tc.method, tc.path, func(req *recordedRequest) mockResponse {
				response := jsonResponse(tc.status, map[string]string{"code": tc.code, "message": "forged"})
				tc.tamper(&response)
				return response
			})

			err := tc.action(gateway)
			require.Error(t, err, "an unauthenticated provider error must never be acted on")
			if tc.notSentinel != nil {
				assert.NotErrorIs(t, err, tc.notSentinel)
			}
			assert.False(t, ierr.IsNotFound(err))
			assert.True(t, ierr.IsHTTPClient(err), "unverified errors stay uncertain provider errors, got %v", err)
		})
	}
}

// TestSignedUnknownNotFoundCodesStayUncertain pins the narrowing of not-found
// detection: even a correctly signed 404 proves nothing unless the body
// carries the provider's official code for that resource (ORDER_NOT_EXIST for
// orders, RESOURCE_NOT_EXISTS for refunds, each for its own family). Anything
// else stays a generic, uncertain provider error.
func TestSignedUnknownNotFoundCodesStayUncertain(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		path        string
		code        string
		action      func(gateway Gateway) error
		notSentinel error
	}{
		{
			name: "order query unknown code", method: http.MethodGet, path: testQueryPath, code: "NOT_FOUND",
			action: func(gateway Gateway) error {
				_, err := gateway.GetTransaction(testContext(), "pay_01TESTPAYMENT")
				return err
			},
			notSentinel: ErrOrderNotFound,
		},
		{
			name: "order query wrong family code", method: http.MethodGet, path: testQueryPath, code: "RESOURCE_NOT_EXISTS",
			action: func(gateway Gateway) error {
				_, err := gateway.GetTransaction(testContext(), "pay_01TESTPAYMENT")
				return err
			},
			notSentinel: ErrOrderNotFound,
		},
		{
			name: "refund query unknown code", method: http.MethodGet, path: testRefundPath, code: "NOT_FOUND",
			action: func(gateway Gateway) error {
				_, err := gateway.GetRefund(testContext(), "refund_01TESTREFUND")
				return err
			},
			notSentinel: ErrRefundNotFound,
		},
		{
			name: "refund query wrong family code", method: http.MethodGet, path: testRefundPath, code: "ORDER_NOT_EXIST",
			action: func(gateway Gateway) error {
				_, err := gateway.GetRefund(testContext(), "refund_01TESTREFUND")
				return err
			},
			notSentinel: ErrRefundNotFound,
		},
		{
			name: "close order unknown code", method: http.MethodPost, path: testClosePath, code: "NOT_FOUND",
			action:      func(gateway Gateway) error { return gateway.CloseOrder(testContext(), "pay_01TESTPAYMENT") },
			notSentinel: ErrOrderNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, gateway, _ := newTestGateway(t)
			mock.handle(tc.method, tc.path, func(req *recordedRequest) mockResponse {
				return jsonResponse(http.StatusNotFound, map[string]string{"code": tc.code, "message": "signed but unrecognised"})
			})

			err := tc.action(gateway)
			require.Error(t, err)
			assert.NotErrorIs(t, err, tc.notSentinel)
			assert.False(t, ierr.IsNotFound(err))
			assert.True(t, ierr.IsHTTPClient(err), "only the official not-found code may map, got %v", err)
		})
	}
}
