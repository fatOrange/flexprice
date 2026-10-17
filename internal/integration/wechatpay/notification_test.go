package wechatpay

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wechatpay-apiv3/wechatpay-go/utils"
)

// testNotificationNonce is a 12-byte AES-GCM nonce, the size WeChat Pay uses
// on the wire and the size cipher.GCM requires.
const testNotificationNonce = "mocknonce123"

// encryptNotificationResource produces the AEAD_AES_256_GCM fields WeChat Pay
// puts in a notification resource: the APIv3 key as the AES key, the resource
// nonce and associated data as given.
func encryptNotificationResource(t *testing.T, payload any) (ciphertext, nonce, associatedData string) {
	t.Helper()
	plaintext, err := json.Marshal(payload)
	require.NoError(t, err)
	block, err := aes.NewCipher([]byte(testAPIv3Key))
	require.NoError(t, err)
	aead, err := cipher.NewGCM(block)
	require.NoError(t, err)
	associatedData = "mock-associated-data"
	sealed := aead.Seal(nil, []byte(testNotificationNonce), plaintext, []byte(associatedData))
	return base64.StdEncoding.EncodeToString(sealed), testNotificationNonce, associatedData
}

// buildNotification wraps an encrypted resource in WeChat Pay's callback
// envelope and signs the exact body bytes with the platform key.
func buildNotification(t *testing.T, keys *testKeys, eventType, originalType string, payload any) (http.Header, []byte) {
	t.Helper()
	ciphertext, nonce, associatedData := encryptNotificationResource(t, payload)
	envelope := map[string]any{
		"id":            "EV-202610120000000001",
		"create_time":   "2026-10-12T10:00:00+08:00",
		"event_type":    eventType,
		"resource_type": "encrypt-resource",
		"resource": map[string]any{
			"algorithm":       "AEAD_AES_256_GCM",
			"ciphertext":      ciphertext,
			"nonce":           nonce,
			"associated_data": associatedData,
			"original_type":   originalType,
		},
		"summary": "mock notification",
	}
	body, err := json.Marshal(envelope)
	require.NoError(t, err)
	return signedNotificationHeaders(t, keys, strconv.FormatInt(time.Now().Unix(), 10), body), body
}

// signedNotificationHeaders signs a callback body the way WeChat Pay does:
// RSA-SHA256 over timestamp\nnonce\nbody\n, keyed by the platform private key.
func signedNotificationHeaders(t *testing.T, keys *testKeys, timestamp string, body []byte) http.Header {
	t.Helper()
	nonce := "notificationheader"
	signature, err := utils.SignSHA256WithRSA(timestamp+"\n"+nonce+"\n"+string(body)+"\n", keys.platformPrivate)
	require.NoError(t, err)
	headers := http.Header{}
	headers.Set("Wechatpay-Timestamp", timestamp)
	headers.Set("Wechatpay-Nonce", nonce)
	headers.Set("Wechatpay-Serial", testPlatformKeyID)
	headers.Set("Wechatpay-Signature", signature)
	return headers
}

func notificationGateway(t *testing.T) (Gateway, *testKeys) {
	t.Helper()
	keys := generateTestKeys(t)
	mock := newSignedMockGateway(t, keys)
	return mock.client(t, &fakeConnectionRepo{conn: mock.connection(t, keys)}), keys
}

func TestParseNotificationTransaction(t *testing.T) {
	gateway, keys := notificationGateway(t)
	payload := Transaction{
		AppID:         testAppID,
		MerchantID:    testMerchantID,
		OutTradeNo:    "pay_01TESTPAYMENT",
		TransactionID: "4200001234202610121234567890",
		TradeState:    TradeStateSuccess,
		SuccessTime:   "2026-10-12T10:00:00+08:00",
		Amount:        TransactionAmount{Total: 1234, Currency: "CNY"},
	}
	headers, body := buildNotification(t, keys, EventTypeTransactionSuccess, resourceTypeTransaction, payload)

	notification, err := gateway.ParseNotification(testContext(), headers, body)
	require.NoError(t, err)
	assert.Equal(t, EventTypeTransactionSuccess, notification.EventType)
	require.NotNil(t, notification.Transaction)
	assert.Nil(t, notification.Refund)
	assert.Equal(t, testAppID, notification.Transaction.AppID)
	assert.Equal(t, testMerchantID, notification.Transaction.MerchantID)
	assert.Equal(t, "pay_01TESTPAYMENT", notification.Transaction.OutTradeNo)
	assert.Equal(t, "4200001234202610121234567890", notification.Transaction.TransactionID)
	assert.Equal(t, TradeStateSuccess, notification.Transaction.TradeState)
	assert.Equal(t, int64(1234), notification.Transaction.Amount.Total)
	assert.Equal(t, "CNY", notification.Transaction.Amount.Currency)
}

func TestParseNotificationRefund(t *testing.T) {
	gateway, keys := notificationGateway(t)
	payload := Refund{
		OutTradeNo:    "pay_01TESTPAYMENT",
		TransactionID: "4200001234202610121234567890",
		OutRefundNo:   "refund_01TESTREFUND",
		RefundID:      "5000000038202610121234567890",
		RefundStatus:  RefundStatusSuccess,
		SuccessTime:   "2026-10-12T10:05:00+08:00",
		Amount:        RefundAmount{Total: 1234, Refund: 1234, Currency: "CNY"},
	}
	headers, body := buildNotification(t, keys, EventTypeRefundSuccess, resourceTypeRefund, payload)

	notification, err := gateway.ParseNotification(testContext(), headers, body)
	require.NoError(t, err)
	assert.Equal(t, EventTypeRefundSuccess, notification.EventType)
	require.NotNil(t, notification.Refund)
	assert.Nil(t, notification.Transaction)
	assert.Equal(t, "refund_01TESTREFUND", notification.Refund.OutRefundNo)
	assert.Equal(t, "5000000038202610121234567890", notification.Refund.RefundID)
	assert.Equal(t, RefundStatusSuccess, notification.Refund.RefundStatus)
	assert.Equal(t, int64(1234), notification.Refund.Amount.Refund)
}

func TestParseNotificationUnknownEventIsAcknowledgeable(t *testing.T) {
	gateway, keys := notificationGateway(t)
	headers, body := buildNotification(t, keys, "ACCOUNT.ALERT", "account", map[string]any{"alert_type": "test"})

	notification, err := gateway.ParseNotification(testContext(), headers, body)
	require.NoError(t, err)
	assert.Equal(t, "ACCOUNT.ALERT", notification.EventType)
	assert.Nil(t, notification.Transaction)
	assert.Nil(t, notification.Refund)
}

func TestParseNotificationRejectsTamperedBody(t *testing.T) {
	gateway, keys := notificationGateway(t)
	payload := Transaction{OutTradeNo: "pay_01TESTPAYMENT", TradeState: TradeStateSuccess}
	headers, body := buildNotification(t, keys, EventTypeTransactionSuccess, resourceTypeTransaction, payload)

	// Flip one byte inside the signed body: the signature must no longer match.
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)/2] ^= 0x01

	_, err := gateway.ParseNotification(testContext(), headers, tampered)
	require.Error(t, err)
	assert.True(t, ierr.IsPermissionDenied(err), "expected permission denied, got %v", err)
}

func TestParseNotificationRejectsWrongPlatformSerial(t *testing.T) {
	gateway, keys := notificationGateway(t)
	headers, body := buildNotification(t, keys, EventTypeTransactionSuccess, resourceTypeTransaction, Transaction{})
	headers.Set("Wechatpay-Serial", "PUB_KEY_ID_9999999999999999999999999999999999")

	_, err := gateway.ParseNotification(testContext(), headers, body)
	require.Error(t, err)
	assert.True(t, ierr.IsPermissionDenied(err), "expected permission denied, got %v", err)
}

func TestParseNotificationRejectsStaleTimestamp(t *testing.T) {
	gateway, keys := notificationGateway(t)
	payload := Transaction{OutTradeNo: "pay_01TESTPAYMENT"}
	ciphertext, nonce, associatedData := encryptNotificationResource(t, payload)
	envelope := map[string]any{
		"id":            "EV-202610120000000001",
		"create_time":   "2026-10-12T10:00:00+08:00",
		"event_type":    EventTypeTransactionSuccess,
		"resource_type": "encrypt-resource",
		"resource": map[string]any{
			"algorithm":       "AEAD_AES_256_GCM",
			"ciphertext":      ciphertext,
			"nonce":           nonce,
			"associated_data": associatedData,
			"original_type":   resourceTypeTransaction,
		},
	}
	body, err := json.Marshal(envelope)
	require.NoError(t, err)
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	headers := signedNotificationHeaders(t, keys, stale, body)

	_, err = gateway.ParseNotification(testContext(), headers, body)
	require.Error(t, err)
	assert.True(t, ierr.IsPermissionDenied(err), "expected permission denied, got %v", err)
}

func TestParseNotificationRejectsInconsistentResourceType(t *testing.T) {
	gateway, keys := notificationGateway(t)
	headers, body := buildNotification(t, keys, EventTypeRefundSuccess, resourceTypeTransaction, Transaction{})

	_, err := gateway.ParseNotification(testContext(), headers, body)
	require.Error(t, err)
	assert.True(t, ierr.IsValidation(err), "expected validation error, got %v", err)
}

func TestParseNotificationRejectsWrongAPIv3Key(t *testing.T) {
	gateway, keys := notificationGateway(t)
	payload := Transaction{OutTradeNo: "pay_01TESTPAYMENT"}
	plaintext, err := json.Marshal(payload)
	require.NoError(t, err)
	block, err := aes.NewCipher([]byte("fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)
	aead, err := cipher.NewGCM(block)
	require.NoError(t, err)
	sealed := aead.Seal(nil, []byte(testNotificationNonce), plaintext, []byte("mock-associated-data"))
	envelope := map[string]any{
		"id":            "EV-202610120000000001",
		"create_time":   "2026-10-12T10:00:00+08:00",
		"event_type":    EventTypeTransactionSuccess,
		"resource_type": "encrypt-resource",
		"resource": map[string]any{
			"algorithm":       "AEAD_AES_256_GCM",
			"ciphertext":      base64.StdEncoding.EncodeToString(sealed),
			"nonce":           testNotificationNonce,
			"associated_data": "mock-associated-data",
			"original_type":   resourceTypeTransaction,
		},
	}
	body, err := json.Marshal(envelope)
	require.NoError(t, err)
	headers := signedNotificationHeaders(t, keys, strconv.FormatInt(time.Now().Unix(), 10), body)

	_, err = gateway.ParseNotification(testContext(), headers, body)
	require.Error(t, err)
	assert.True(t, ierr.IsPermissionDenied(err), "expected permission denied, got %v", err)
}

func TestParseNotificationRejectsEmptyBody(t *testing.T) {
	gateway, _ := notificationGateway(t)
	_, err := gateway.ParseNotification(testContext(), http.Header{}, nil)
	require.Error(t, err)
	assert.True(t, ierr.IsValidation(err), "expected validation error, got %v", err)
}

func TestParseNotificationRejectsInvalidNonceLength(t *testing.T) {
	cases := []struct {
		name  string
		nonce string
	}{
		{name: "16 byte nonce", nonce: "1234567890123456"},
		{name: "empty nonce", nonce: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gateway, keys := notificationGateway(t)
			_, body := buildNotification(t, keys, EventTypeTransactionSuccess, resourceTypeTransaction, Transaction{OutTradeNo: "pay_01TESTPAYMENT"})

			// A structurally valid, correctly signed callbacks whose resource
			// carries a nonce the SDK's GCM would panic on: reject it before
			// the SDK ever runs cipher.Open.
			var envelope map[string]any
			require.NoError(t, json.Unmarshal(body, &envelope))
			resource, ok := envelope["resource"].(map[string]any)
			require.True(t, ok)
			resource["nonce"] = tc.nonce
			body, err := json.Marshal(envelope)
			require.NoError(t, err)
			headers := signedNotificationHeaders(t, keys, strconv.FormatInt(time.Now().Unix(), 10), body)

			notification, err := gateway.ParseNotification(testContext(), headers, body)
			require.Error(t, err)
			assert.Nil(t, notification)
			assert.True(t, ierr.IsValidation(err), "expected validation error, got %v", err)
		})
	}
}

func TestParseNotificationRejectionErrorIsSanitized(t *testing.T) {
	gateway, keys := notificationGateway(t)
	headers, body := buildNotification(t, keys, EventTypeTransactionSuccess, resourceTypeTransaction, Transaction{OutTradeNo: "pay_01TESTPAYMENT"})

	// Truncating the signature guarantees verification fails without touching
	// the body, so the failure stays a signature failure.
	signature := headers.Get("Wechatpay-Signature")
	require.NotEmpty(t, signature)
	headers.Set("Wechatpay-Signature", signature[:len(signature)-4])

	notification, err := gateway.ParseNotification(testContext(), headers, body)
	require.Error(t, err)
	assert.Nil(t, notification)
	assert.True(t, ierr.IsPermissionDenied(err), "expected permission denied, got %v", err)

	// The failure stays classified, and the SDK error — which embeds the whole
	// request: signature headers and the encrypted body — must not survive into
	// the returned error.
	message := err.Error()
	assert.Contains(t, message, "signature_verification_failed", "the failure category must survive sanitization")
	assert.NotContains(t, message, "Wechatpay-Signature")
	assert.NotContains(t, message, string(body))
}
