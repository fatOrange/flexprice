package wechatpay

// Trade states WeChat Pay reports for a transaction (query response and
// TRANSACTION.* notifications). These are WeChat's wire values.
const (
	TradeStateSuccess    = "SUCCESS"
	TradeStateRefund     = "REFUND"
	TradeStateNotPay     = "NOTPAY"
	TradeStateClosed     = "CLOSED"
	TradeStateRevoked    = "REVOKED"
	TradeStateUserPaying = "USERPAYING"
	TradeStatePayError   = "PAYERROR"
)

// Refund statuses WeChat Pay reports for a refund (query response and
// REFUND.* notifications). ABNORMAL needs manual handling before it can
// settle; PROCESSING is an accepted refund whose outcome is still to come.
const (
	RefundStatusSuccess    = "SUCCESS"
	RefundStatusClosed     = "CLOSED"
	RefundStatusProcessing = "PROCESSING"
	RefundStatusAbnormal   = "ABNORMAL"
)

// Notification event types this integration handles. Anything else is
// returned with an empty payload and no typed event so the caller can
// acknowledge it without acting on it.
const (
	EventTypeTransactionSuccess = "TRANSACTION.SUCCESS"
	EventTypeRefundSuccess      = "REFUND.SUCCESS"
	EventTypeRefundAbnormal     = "REFUND.ABNORMAL"
	EventTypeRefundClosed       = "REFUND.CLOSED"
)

const (
	eventTypeTransactionPrefix = "TRANSACTION."
	eventTypeRefundPrefix      = "REFUND."
	resourceTypeTransaction    = "transaction"
	resourceTypeRefund         = "refund"
)

// Transaction is one WeChat Pay transaction as reported by the signed query
// API or a verified TRANSACTION.* notification. Field names and JSON keys
// follow WeChat's wire format exactly; amounts are integer fen (CNY cents).
type Transaction struct {
	AppID         string            `json:"appid"`
	MerchantID    string            `json:"mchid"`
	OutTradeNo    string            `json:"out_trade_no"`
	TransactionID string            `json:"transaction_id"`
	TradeState    string            `json:"trade_state"`
	SuccessTime   string            `json:"success_time,omitempty"`
	Amount        TransactionAmount `json:"amount"`
}

// TransactionAmount is the transaction amount block. Total is in fen.
type TransactionAmount struct {
	Total    int64  `json:"total"`
	Currency string `json:"currency"`
}

// Refund is one WeChat Pay refund as reported by the signed query API or a
// verified REFUND.* notification. Total and Refund are in fen.
type Refund struct {
	OutTradeNo    string       `json:"out_trade_no"`
	TransactionID string       `json:"transaction_id"`
	OutRefundNo   string       `json:"out_refund_no"`
	RefundID      string       `json:"refund_id"`
	RefundStatus  string       `json:"status"`
	SuccessTime   string       `json:"success_time,omitempty"`
	Amount        RefundAmount `json:"amount"`
}

// RefundAmount is the refund amount block. Total is the original order amount
// and Refund the amount returned, both in fen.
type RefundAmount struct {
	Total    int64  `json:"total"`
	Refund   int64  `json:"refund"`
	Currency string `json:"currency"`
}

// Notification is a verified and decrypted WeChat Pay callback. Exactly one of
// Transaction or Refund is set for the event types this integration handles;
// both are nil for event types it does not act on, which the caller can
// acknowledge and ignore.
type Notification struct {
	EventType   string       `json:"event_type"`
	Transaction *Transaction `json:"transaction,omitempty"`
	Refund      *Refund      `json:"refund,omitempty"`
}
