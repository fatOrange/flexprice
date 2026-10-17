package wechatpay

import (
	"context"

	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/interfaces"
)

// refundGateway is the package-private capability the refund adapter needs.
// The public Gateway contract is query-only for refunds; creating one
// additionally needs the signed SDK client and connection configuration that
// only the concrete client can mint, so the adapter reaches the concrete
// implementation through this capability instead of widening the public
// interface.
type refundGateway interface {
	refundPayment(ctx context.Context, req interfaces.RefundProviderRequest) (*interfaces.RefundProviderResponse, error)
}

type refundAdapter struct {
	refunds refundGateway
}

// NewRefundAdapter wraps a WeChat Pay gateway as a refund provider.
func NewRefundAdapter(gateway Gateway) interfaces.RefundProvider {
	adapter := &refundAdapter{}
	if refunds, ok := gateway.(refundGateway); ok {
		adapter.refunds = refunds
	}
	return adapter
}

// RefundPayment issues a full refund of the original transaction. Partial
// refunds are rejected before WeChat Pay ever sees them.
func (a *refundAdapter) RefundPayment(ctx context.Context, req interfaces.RefundProviderRequest) (*interfaces.RefundProviderResponse, error) {
	if a == nil || a.refunds == nil {
		return nil, ierr.NewError("wechatpay refund adapter is not configured").
			WithHint("The WeChat Pay gateway implementation does not support refunds through this adapter").
			Mark(ierr.ErrNotImplemented)
	}
	return a.refunds.refundPayment(ctx, req)
}
