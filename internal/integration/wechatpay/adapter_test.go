package wechatpay

import (
	"context"
	"errors"
	"net/http"
	"testing"

	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/interfaces"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGateway is a scriptable Gateway used to test adapter contracts without
// any network or connection wiring.
type fakeGateway struct {
	createPayment  func(ctx context.Context, req interfaces.CheckoutProviderRequest, options types.WechatPayOptions) (*interfaces.CheckoutProviderResponse, error)
	transaction    *Transaction
	transactionErr error
	queriedOrders  []string
}

func (f *fakeGateway) CreatePayment(ctx context.Context, req interfaces.CheckoutProviderRequest, options types.WechatPayOptions) (*interfaces.CheckoutProviderResponse, error) {
	if f.createPayment == nil {
		return nil, errors.New("unexpected CreatePayment call")
	}
	return f.createPayment(ctx, req, options)
}

func (f *fakeGateway) GetTransaction(ctx context.Context, orderID string) (*Transaction, error) {
	f.queriedOrders = append(f.queriedOrders, orderID)
	return f.transaction, f.transactionErr
}

func (f *fakeGateway) GetTransactionByID(ctx context.Context, transactionID string) (*Transaction, error) {
	f.queriedOrders = append(f.queriedOrders, transactionID)
	return f.transaction, f.transactionErr
}

func (f *fakeGateway) CloseOrder(ctx context.Context, orderID string) error {
	return nil
}

func (f *fakeGateway) ParseNotification(ctx context.Context, headers http.Header, rawBody []byte) (*Notification, error) {
	return nil, errors.New("unexpected ParseNotification call")
}

func (f *fakeGateway) GetRefund(ctx context.Context, outRefundNo string) (*Refund, error) {
	return nil, ErrRefundNotFound
}

func TestCheckoutAdapterDefaultsToNative(t *testing.T) {
	var seen types.WechatPayOptions
	gateway := &fakeGateway{createPayment: func(ctx context.Context, req interfaces.CheckoutProviderRequest, options types.WechatPayOptions) (*interfaces.CheckoutProviderResponse, error) {
		seen = options
		return &interfaces.CheckoutProviderResponse{ProviderSessionID: req.PaymentID}, nil
	}}
	adapter := NewCheckoutAdapter(gateway)

	req := testCheckoutRequest()
	resp, err := adapter.CreatePaymentLink(testContext(), req)
	require.NoError(t, err)
	assert.Equal(t, req.PaymentID, resp.ProviderSessionID)
	assert.Equal(t, types.WechatPayPaymentTypeNative, seen.EffectivePaymentType())
}

func TestCheckoutAdapterPassesJSAPIOptions(t *testing.T) {
	var seen types.WechatPayOptions
	gateway := &fakeGateway{createPayment: func(ctx context.Context, req interfaces.CheckoutProviderRequest, options types.WechatPayOptions) (*interfaces.CheckoutProviderResponse, error) {
		seen = options
		return &interfaces.CheckoutProviderResponse{}, nil
	}}
	adapter := NewCheckoutAdapter(gateway)

	req := testCheckoutRequest()
	options := &types.WechatPayOptions{PaymentType: types.WechatPayPaymentTypeJSAPI, OpenID: "oTestOpenID"}
	req.WechatPayOptions = options

	_, err := adapter.CreatePaymentLink(testContext(), req)
	require.NoError(t, err)
	assert.Equal(t, "oTestOpenID", seen.OpenID)
	assert.True(t, seen.IsJSAPI())
}

func TestCheckoutAdapterRejectsInstrumentRegistration(t *testing.T) {
	adapter := NewCheckoutAdapter(&fakeGateway{})
	_, err := adapter.CreateAuthorizationLink(testContext(), interfaces.AuthorizationLinkRequest{})
	require.Error(t, err)
	assert.True(t, ierr.IsNotImplemented(err), "expected not implemented, got %v", err)
}

func TestCheckoutAdapterTryAutoChargeReportsNoMethod(t *testing.T) {
	adapter := NewCheckoutAdapter(&fakeGateway{})
	resp, charged, err := adapter.TryAutoChargingSavedMethod(testContext(), interfaces.AuthorizationLinkRequest{})
	require.NoError(t, err)
	assert.Nil(t, resp)
	assert.False(t, charged)
}

func TestCheckoutAdapterHasAutoChargeableMethodNotImplemented(t *testing.T) {
	adapter := NewCheckoutAdapter(&fakeGateway{})
	_, err := adapter.HasAutoChargeableMethod(testContext(), interfaces.HasAutoChargeableMethodRequest{})
	require.Error(t, err)
	assert.True(t, ierr.IsNotImplemented(err), "expected not implemented, got %v", err)
}

func TestCheckoutAdapterFetchPaymentState(t *testing.T) {
	cases := []struct {
		name        string
		tradeState  string
		wantStatus  types.PaymentStatus
		wantGateway string
	}{
		{name: "success", tradeState: TradeStateSuccess, wantStatus: types.PaymentStatusSucceeded, wantGateway: testTransactionID},
		{name: "refunded", tradeState: TradeStateRefund, wantStatus: types.PaymentStatusRefunded, wantGateway: testTransactionID},
		{name: "closed", tradeState: TradeStateClosed, wantStatus: types.PaymentStatusFailed},
		{name: "revoked", tradeState: TradeStateRevoked, wantStatus: types.PaymentStatusFailed},
		{name: "pay error", tradeState: TradeStatePayError, wantStatus: types.PaymentStatusFailed},
		{name: "not pay", tradeState: TradeStateNotPay, wantStatus: ""},
		{name: "user paying", tradeState: TradeStateUserPaying, wantStatus: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &fakeGateway{transaction: &Transaction{
				TradeState:    tc.tradeState,
				TransactionID: testTransactionID,
			}}
			adapter := NewCheckoutAdapter(gateway)

			state, err := adapter.FetchPaymentState(testContext(), interfaces.PaymentStateRequest{GatewayTrackingID: "pay_01TESTPAYMENT"})
			require.NoError(t, err)
			require.NotNil(t, state)
			assert.Equal(t, tc.wantStatus, state.Status)
			assert.Equal(t, tc.wantGateway, state.GatewayPaymentID)
			assert.Equal(t, []string{"pay_01TESTPAYMENT"}, gateway.queriedOrders)
		})
	}
}

func TestCheckoutAdapterFetchPaymentStateByTransactionID(t *testing.T) {
	gateway := &fakeGateway{transaction: &Transaction{TradeState: TradeStateSuccess, TransactionID: testTransactionID}}
	adapter := NewCheckoutAdapter(gateway)

	state, err := adapter.FetchPaymentState(testContext(), interfaces.PaymentStateRequest{GatewayPaymentID: testTransactionID})
	require.NoError(t, err)
	assert.Equal(t, types.PaymentStatusSucceeded, state.Status)
	assert.Equal(t, []string{testTransactionID}, gateway.queriedOrders)
}

func TestCheckoutAdapterFetchPaymentStateWithoutHandles(t *testing.T) {
	adapter := NewCheckoutAdapter(&fakeGateway{})
	state, err := adapter.FetchPaymentState(testContext(), interfaces.PaymentStateRequest{InvoiceID: "inv_01TESTINVOICE"})
	require.NoError(t, err)
	assert.Nil(t, state)
}

func TestCheckoutAdapterPropagatesGatewayErrors(t *testing.T) {
	gateway := &fakeGateway{transactionErr: ErrOrderNotFound}
	adapter := NewCheckoutAdapter(gateway)

	_, err := adapter.FetchPaymentState(testContext(), interfaces.PaymentStateRequest{GatewayTrackingID: "pay_01TESTPAYMENT"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOrderNotFound)
}

func TestAdaptersRejectNilGateway(t *testing.T) {
	_, err := NewCheckoutAdapter(nil).CreatePaymentLink(testContext(), testCheckoutRequest())
	require.Error(t, err)
	assert.True(t, ierr.IsNotImplemented(err), "expected not implemented, got %v", err)

	_, err = NewCheckoutAdapter(nil).FetchPaymentState(testContext(), interfaces.PaymentStateRequest{GatewayTrackingID: "x"})
	require.Error(t, err)
	assert.True(t, ierr.IsNotImplemented(err), "expected not implemented, got %v", err)
}
