package wechatpay

import "errors"

// Sentinel errors the caller branches on. They survive the internal error
// chain, so errors.Is(err, wechatpay.ErrOrderNotFound) holds even though the
// returned error also carries an ierr code and hints.
var (
	// ErrOrderNotFound reports that WeChat Pay has no record of the merchant
	// order, for example when a prepay never reached WeChat. The payment
	// attempt is untouched by WeChat Pay and can be failed locally.
	ErrOrderNotFound = errors.New("wechatpay: order not found")

	// ErrOrderPaid reports that a close order request lost the race with a
	// payment: WeChat Pay refuses to close an order that is already paid. The
	// caller must query the order and settle it instead of closing.
	ErrOrderPaid = errors.New("wechatpay: order already paid")

	// ErrRefundNotFound reports that WeChat Pay has no record of the refund
	// for the given merchant refund number.
	ErrRefundNotFound = errors.New("wechatpay: refund not found")
)
