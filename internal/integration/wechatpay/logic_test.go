package wechatpay

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The merchant order/refund number derivation is exported API the application
// binds before the first network call and reuses on every retry, so it is
// tested directly here. The remaining protocol behavior is exercised through
// the Gateway against a signed loopback provider in client_test.go,
// notification_test.go, and adapter_test.go.

func TestMerchantOrderIDPrefersTheFlexPriceID(t *testing.T) {
	assert.Equal(t, "pay_01TESTPAYMENT", MerchantOrderID("pay_01TESTPAYMENT"))
	assert.Equal(t, MerchantOrderID("pay_01TESTPAYMENT"), MerchantOrderID("pay_01TESTPAYMENT"))
	assert.Equal(t, "", MerchantOrderID(""))
}

func TestMerchantOrderIDHashesUnusableIDs(t *testing.T) {
	short := MerchantOrderID("abc")
	assert.Len(t, short, merchantOrderIDMaxLength)
	assert.True(t, strings.HasPrefix(short, "wx"))
	assert.True(t, validMerchantNumber(short, merchantOrderIDMinLength, merchantOrderIDMaxLength))
	assert.NotEqual(t, short, MerchantOrderID("abd"), "different ids must not collide")

	illegal := MerchantOrderID("pay/01 with spaces")
	assert.True(t, validMerchantNumber(illegal, merchantOrderIDMinLength, merchantOrderIDMaxLength))
	assert.NotContains(t, illegal, "/")

	long := MerchantOrderID(strings.Repeat("x", merchantOrderIDMaxLength+1))
	assert.Len(t, long, merchantOrderIDMaxLength)
	assert.True(t, validMerchantNumber(long, merchantOrderIDMinLength, merchantOrderIDMaxLength))
}

func TestMerchantRefundNoPrefersTheFlexPriceID(t *testing.T) {
	assert.Equal(t, "refund_01TESTREFUND", MerchantRefundNo("refund_01TESTREFUND"))
	assert.Equal(t, "", MerchantRefundNo(""))

	hashed := MerchantRefundNo("r")
	assert.True(t, strings.HasPrefix(hashed, "wr"))
	assert.Len(t, hashed, merchantRefundNoMaxLength)
	assert.True(t, validMerchantNumber(hashed, merchantRefundNoMinLength, merchantRefundNoMaxLength))
}
