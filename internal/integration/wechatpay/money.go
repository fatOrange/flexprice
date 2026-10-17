package wechatpay

import (
	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/shopspring/decimal"
)

// currencyCNY is the only currency WeChat Pay settles for this merchant class.
const currencyCNY = "CNY"

// amountToFen converts a decimal major-unit amount to whole fen (CNY cents)
// for the wire. WeChat Pay has no sub-fen precision, so fractional fen, zero
// and negative amounts, and values that overflow int64 fen are all rejected
// before any network call.
func amountToFen(amount decimal.Decimal, currency string) (int64, error) {
	if !types.IsMatchingCurrency(currency, currencyCNY) {
		return 0, ierr.NewError("wechatpay only supports CNY").
			WithHint("WeChat Pay settles in CNY only; convert or route this payment to another gateway").
			WithReportableDetails(map[string]any{"currency": currency}).
			Mark(ierr.ErrValidation)
	}
	if !amount.IsPositive() {
		return 0, ierr.NewError("wechatpay amount must be greater than zero").
			WithHint("WeChat Pay requires a positive amount").
			WithReportableDetails(map[string]any{"amount": amount.String()}).
			Mark(ierr.ErrValidation)
	}

	// Multiplication by 100 is exact for decimals, so any rounding here would
	// come from the caller's exponent, never from this conversion.
	scaled := amount.Mul(decimal.New(1, 2))
	if !scaled.IsInteger() {
		return 0, ierr.NewError("wechatpay amount must be a whole number of fen").
			WithHint("WeChat Pay settles CNY in whole fen; round the amount to two decimal places").
			WithReportableDetails(map[string]any{"amount": amount.String()}).
			Mark(ierr.ErrValidation)
	}
	fen := scaled.BigInt()
	if !fen.IsInt64() {
		return 0, ierr.NewError("wechatpay amount is too large").
			WithHint("The amount exceeds the largest order WeChat Pay can represent in fen").
			WithReportableDetails(map[string]any{"amount": amount.String()}).
			Mark(ierr.ErrValidation)
	}
	return fen.Int64(), nil
}

// fenToAmount converts integer fen back to a decimal CNY amount.
func fenToAmount(fen int64) decimal.Decimal {
	return types.FromSmallestUnit(fen, currencyCNY)
}
