package wechatpay

import (
	"crypto/sha256"
	"encoding/hex"
)

// WeChat Pay merchant order/refund numbers accept the characters below only,
// and bound the merchant order number to 32 and the merchant refund number to
// 64 characters. FlexPrice ids are prefixed ULIDs, which fit as they are; ids
// that do not fit are replaced by a deterministic digest so retries of the
// same attempt always address the same WeChat order.
const (
	merchantOrderIDMinLength  = 6
	merchantOrderIDMaxLength  = 32
	merchantRefundNoMinLength = 6
	merchantRefundNoMaxLength = 64
)

func validMerchantNumber(s string, minLength, maxLength int) bool {
	if len(s) < minLength || len(s) > maxLength {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c == '_' || c == '-' || c == '|' || c == '*' || c == '@':
		default:
			return false
		}
	}
	return true
}

// MerchantOrderID derives the stable WeChat Pay out_trade_no for a FlexPrice
// payment. The caller binds it as the payment's gateway tracking id before the
// first network call and must reuse it on every retry: WeChat Pay treats a
// different number as a different order. Returns "" for an empty payment id.
func MerchantOrderID(paymentID string) string {
	if paymentID == "" {
		return ""
	}
	if validMerchantNumber(paymentID, merchantOrderIDMinLength, merchantOrderIDMaxLength) {
		return paymentID
	}
	sum := sha256.Sum256([]byte(paymentID))
	return "wx" + hex.EncodeToString(sum[:])[:merchantOrderIDMaxLength-2]
}

// MerchantRefundNo derives the stable WeChat Pay out_refund_no for a refund
// row id. One payment can be refunded more than once, so the caller must pass
// the same key it gave to RefundPayment when querying the refund back. Returns
// "" for an empty key.
func MerchantRefundNo(refundKey string) string {
	if refundKey == "" {
		return ""
	}
	if validMerchantNumber(refundKey, merchantRefundNoMinLength, merchantRefundNoMaxLength) {
		return refundKey
	}
	sum := sha256.Sum256([]byte(refundKey))
	return "wr" + hex.EncodeToString(sum[:])[:merchantRefundNoMaxLength-2]
}
