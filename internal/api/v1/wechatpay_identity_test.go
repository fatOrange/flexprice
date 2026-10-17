package v1

import (
	"testing"

	"github.com/flexprice/flexprice/internal/types"
	"github.com/stretchr/testify/require"

	wechatsim "github.com/flexprice/flexprice/cmd/wechatpay-smoke/wechatsim"
)

// TestWechatPayQueryPaymentRejectsWrongIdentityWithoutClosing proves the query
// path verifies the transaction's identity (mchid/appid/out_trade_no against
// the payment and its tenant connection) before trusting any state claim, and
// that a mismatch is terminal for reconciliation — no settlement, no close,
// no mutation — even when the response claims SUCCESS or CLOSED.
func TestWechatPayQueryPaymentRejectsWrongIdentityWithoutClosing(t *testing.T) {
	cases := []struct {
		name     string
		override wechatsim.QueryOverride
		expired  bool
	}{
		{
			name: "wrong appid claiming SUCCESS past expiry",
			override: wechatsim.QueryOverride{
				AppID:      new("wxfakeotherapp00001"),
				TradeState: new("SUCCESS"),
			},
			expired: true,
		},
		{
			name: "wrong mchid claiming CLOSED past expiry",
			override: wechatsim.QueryOverride{
				MchID:      new("1900000002"),
				TradeState: new("CLOSED"),
			},
			expired: true,
		},
		{
			name: "wrong out_trade_no claiming SUCCESS",
			override: wechatsim.QueryOverride{
				OutTradeNo: new("wx-identity-mismatch"),
				TradeState: new("SUCCESS"),
			},
			expired: false,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stack := newWechatPayTestStack(t)
			invoiceID := "inv_wp_identity_" + string(rune('a'+i))
			paymentID := "pay_wp_identity_" + string(rune('a'+i))
			stack.seedFinalizedInvoice(t, invoiceID, "100.00")
			row := stack.seedOrderAndPayment(t, paymentID, invoiceID, "100.00")
			orderID := *row.GatewayTrackingID
			if tc.expired {
				stack.stageExpiry(t, row.ID)
			}
			stack.sim.SetQueryOverride(orderID, tc.override)

			_, err := stack.wechatService.QueryPayment(stack.ctx, row.ID)
			require.Error(t, err, "a transaction with the wrong identity must surface an error")

			after := stack.mustGetPayment(t, row.ID)
			require.Equal(t, types.PaymentStatusPending, after.PaymentStatus,
				"an identity mismatch must leave the payment in its prior state")
			require.Equal(t, 0, stack.sim.CloseCalls(orderID),
				"an identity mismatch must never trigger a close")
			require.NotEqual(t, types.PaymentStatusSucceeded, stack.mustGetInvoice(t, invoiceID).PaymentStatus)
		})
	}
}

// TestWechatPayQueryPaymentAcceptsNilAmountNotPay covers the official schema's
// legitimate NOTPAY-without-amount shape: unexpired it stays PENDING, and past
// the persisted deadline it still closes exactly once and expires.
func TestWechatPayQueryPaymentAcceptsNilAmountNotPay(t *testing.T) {
	t.Run("stays pending while unexpired", func(t *testing.T) {
		stack := newWechatPayTestStack(t)
		stack.seedFinalizedInvoice(t, "inv_wp_noamt_ok", "100.00")
		row := stack.seedOrderAndPayment(t, "pay_wp_noamt_ok", "inv_wp_noamt_ok", "100.00")
		orderID := *row.GatewayTrackingID
		stack.sim.SetQueryOverride(orderID, wechatsim.QueryOverride{OmitAmount: new(true)})

		queried, err := stack.wechatService.QueryPayment(stack.ctx, row.ID)
		require.NoError(t, err, "a NOTPAY without an amount block is a legitimate response")
		require.Equal(t, types.PaymentStatusPending, queried.PaymentStatus)
		require.Equal(t, 0, stack.sim.CloseCalls(orderID))
		require.NotEqual(t, types.PaymentStatusSucceeded, stack.mustGetInvoice(t, "inv_wp_noamt_ok").PaymentStatus)
	})

	t.Run("closes once past expiry then fails", func(t *testing.T) {
		stack := newWechatPayTestStack(t)
		stack.seedFinalizedInvoice(t, "inv_wp_noamt_exp", "100.00")
		row := stack.seedOrderAndPayment(t, "pay_wp_noamt_exp", "inv_wp_noamt_exp", "100.00")
		orderID := *row.GatewayTrackingID
		stack.stageExpiry(t, row.ID)
		stack.sim.SetQueryOverride(orderID, wechatsim.QueryOverride{OmitAmount: new(true)})

		expired, err := stack.wechatService.QueryPayment(stack.ctx, row.ID)
		require.NoError(t, err)
		require.Equal(t, types.PaymentStatusFailed, expired.PaymentStatus)
		require.NotNil(t, expired.ErrorMessage)
		require.NotEmpty(t, *expired.ErrorMessage, "a failed expiry records why")
		require.Equal(t, 1, stack.sim.CloseCalls(orderID), "an amount-less NOTPAY still closes exactly once past expiry")
		require.NotEqual(t, types.PaymentStatusSucceeded, stack.mustGetInvoice(t, "inv_wp_noamt_exp").PaymentStatus)
	})
}

// TestWechatPayQueryPaymentChecksCloseRaceRequeryIdentity covers the close race:
// the initial honest NOTPAY past expiry triggers a close that the provider
// answers with ORDERPAID; the re-query then disagrees on identity and must again
// leave the payment untouched even though a close has already been attempted.
func TestWechatPayQueryPaymentChecksCloseRaceRequeryIdentity(t *testing.T) {
	stack := newWechatPayTestStack(t)
	stack.seedFinalizedInvoice(t, "inv_wp_closerace", "100.00")
	row := stack.seedOrderAndPayment(t, "pay_wp_closerace", "inv_wp_closerace", "100.00")
	orderID := *row.GatewayTrackingID
	stack.stageExpiry(t, row.ID)
	stack.sim.ForceClosePaidError(orderID)
	// The first query is honest so the close is reached; the re-query claims a
	// SUCCESS for a different merchant.
	stack.sim.SetQueryOverride(orderID, wechatsim.QueryOverride{
		SkipQueries: 1,
		MchID:       new("1900000002"),
		TradeState:  new("SUCCESS"),
	})

	_, _ = stack.wechatService.QueryPayment(stack.ctx, row.ID)

	after := stack.mustGetPayment(t, row.ID)
	require.Equal(t, types.PaymentStatusPending, after.PaymentStatus,
		"a wrong-identity re-query must leave the payment untouched")
	require.Equal(t, 1, stack.sim.CloseCalls(orderID), "the close race attempts exactly one close")
	require.NotEqual(t, types.PaymentStatusSucceeded, stack.mustGetInvoice(t, "inv_wp_closerace").PaymentStatus)
}
