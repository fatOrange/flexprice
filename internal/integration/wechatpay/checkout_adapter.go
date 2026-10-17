package wechatpay

import (
	"context"

	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/interfaces"
	"github.com/flexprice/flexprice/internal/types"
)

// checkoutAdapter exposes the WeChat Pay gateway through the unified checkout
// provider contract. Only manual one-off payments exist for this merchant
// class: a Native QR scan or a JSAPI payment inside the WeChat browser.
type checkoutAdapter struct {
	gateway Gateway
}

// NewCheckoutAdapter wraps a WeChat Pay gateway as a checkout provider.
func NewCheckoutAdapter(gateway Gateway) interfaces.CheckoutProvider {
	return &checkoutAdapter{gateway: gateway}
}

// CreatePaymentLink pre-creates the WeChat Pay order behind an invoice payment.
// The channel comes from the request options and defaults to Native, so a
// caller that sends only an amount gets the desktop QR flow.
func (a *checkoutAdapter) CreatePaymentLink(ctx context.Context, req interfaces.CheckoutProviderRequest) (*interfaces.CheckoutProviderResponse, error) {
	if a == nil || a.gateway == nil {
		return nil, notConfiguredError("checkout")
	}
	var options types.WechatPayOptions
	if req.WechatPayOptions != nil {
		options = *req.WechatPayOptions
	}
	return a.gateway.CreatePayment(ctx, req, options)
}

// CreateAuthorizationLink is unsupported: this integration has no mandate,
// saved-card, or token registration path for WeChat Pay.
func (a *checkoutAdapter) CreateAuthorizationLink(ctx context.Context, req interfaces.AuthorizationLinkRequest) (*interfaces.CheckoutProviderResponse, error) {
	return nil, ierr.NewError("wechatpay does not support payment instrument registration").
		WithHint("WeChat Pay payments are customer-present native or jsapi checkouts; there is no saved-method or mandate path").
		Mark(ierr.ErrNotImplemented)
}

// TryAutoChargingSavedMethod reports that no saved method exists, which routes
// the caller to CreateAuthorizationLink and its explicit not-implemented error
// rather than pretending a charge happened.
func (a *checkoutAdapter) TryAutoChargingSavedMethod(ctx context.Context, req interfaces.AuthorizationLinkRequest) (*interfaces.CheckoutProviderResponse, bool, error) {
	return nil, false, nil
}

// HasAutoChargeableMethod is unsupported: no chargeable instrument ever exists
// for this integration.
func (a *checkoutAdapter) HasAutoChargeableMethod(ctx context.Context, req interfaces.HasAutoChargeableMethodRequest) (bool, error) {
	return false, ierr.NewError("wechatpay does not support saved payment methods").
		WithHint("WeChat Pay payments are customer-present sessions with no stored instrument").
		Mark(ierr.ErrNotImplemented)
}

// FetchPaymentState reads the order state from WeChat Pay so an expired or
// dropped webhook can be reconciled. The merchant order number identifies the
// order until it is paid; afterwards the transaction id is equally
// authoritative, so whichever handle the caller stored is queried directly.
func (a *checkoutAdapter) FetchPaymentState(ctx context.Context, req interfaces.PaymentStateRequest) (*interfaces.PaymentState, error) {
	if a == nil || a.gateway == nil {
		return nil, notConfiguredError("checkout")
	}
	switch {
	case req.GatewayTrackingID != "":
		tx, err := a.gateway.GetTransaction(ctx, req.GatewayTrackingID)
		if err != nil {
			return nil, err
		}
		return paymentStateFromTransaction(tx), nil
	case req.GatewayPaymentID != "":
		tx, err := a.gateway.GetTransactionByID(ctx, req.GatewayPaymentID)
		if err != nil {
			return nil, err
		}
		return paymentStateFromTransaction(tx), nil
	default:
		return nil, nil
	}
}

// paymentStateFromTransaction maps a provider transaction onto the payment
// state a reconciler applies. An empty status means "no transition": the order
// exists but nothing terminal has happened to it.
func paymentStateFromTransaction(tx *Transaction) *interfaces.PaymentState {
	if tx == nil {
		return nil
	}
	state := &interfaces.PaymentState{}
	switch tx.TradeState {
	case TradeStateSuccess:
		state.Status = types.PaymentStatusSucceeded
		state.GatewayPaymentID = tx.TransactionID
	case TradeStateRefund:
		state.Status = types.PaymentStatusRefunded
		state.GatewayPaymentID = tx.TransactionID
	case TradeStateClosed, TradeStateRevoked, TradeStatePayError:
		state.Status = types.PaymentStatusFailed
	}
	return state
}

// notConfiguredError reports an adapter built without a gateway. The factory
// never does that; the guard keeps nil wiring explicit instead of panicking.
func notConfiguredError(kind string) error {
	return ierr.NewError("wechatpay " + kind + " adapter is not configured").
		WithHint("The WeChat Pay client was not injected when the adapter was built").
		Mark(ierr.ErrNotImplemented)
}
