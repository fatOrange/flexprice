package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/integration/wechatpay"
	"github.com/flexprice/flexprice/internal/rest/middleware"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestWechatPayFailedRefundDoesNotFallBackToWallet proves a failed WeChat Pay
// refund is terminal: a verified ABNORMAL/CLOSED outcome (callback or signed
// query) marks the canonical row FAILED with a reason, never mints a fallback
// wallet refund row or wallet transaction, leaves the invoice untouched, and
// cannot be retried automatically.
func TestWechatPayFailedRefundDoesNotFallBackToWallet(t *testing.T) {
	cases := []struct {
		name      string
		mode      string // "callback" | "query"
		eventType string
		status    string
	}{
		{"callback ABNORMAL", "callback", "REFUND.ABNORMAL", "ABNORMAL"},
		{"callback CLOSED", "callback", "REFUND.CLOSED", "CLOSED"},
		{"query ABNORMAL", "query", "", "ABNORMAL"},
		{"query CLOSED", "query", "", "CLOSED"},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stack := newWechatPayTestStack(t)
			suffix := string(rune('a' + i))
			invoiceID := "inv_wp_fail_" + suffix
			paymentID := "pay_wp_fail_" + suffix
			idempotencyKey := "wp-failed-refund-key-" + suffix

			stack.seedFinalizedInvoice(t, invoiceID, "100.00")
			order := stack.seedOrderAndPayment(t, paymentID, invoiceID, "100.00")
			orderID := *order.GatewayTrackingID
			router := stack.callbackRouter()
			stack.settleOrderViaCallback(t, router, orderID, 10000)

			refundRow, providerState := stack.planAndSeedProcessingRefund(t, paymentID, invoiceID, idempotencyKey)
			require.Equal(t, types.RefundStatusProcessing, refundRow.RefundStatus)

			invoiceBefore := stack.mustGetInvoice(t, invoiceID)
			outRefundNo := wechatpay.MerchantRefundNo(idempotencyKey)

			switch tc.mode {
			case "callback":
				headers, body := stack.signedRefundOutcome(tc.eventType, tc.status, orderID, outRefundNo, providerState.RefundID, 10000)
				w := stack.postCallback(router, wpTenantID, wpEnvID, headers, body)
				require.Equal(t, http.StatusOK, w.Code, "callback body: %s", w.Body.String())
			case "query":
				switch tc.status {
				case "ABNORMAL":
					stack.sim.MarkRefundAbnormal(outRefundNo)
				case "CLOSED":
					stack.sim.MarkRefundClosed(outRefundNo)
				}
				_, err := stack.refundService.GetRefund(stack.ctx, refundRow.ID)
				require.NoError(t, err)
			default:
				t.Fatalf("unknown mode %q", tc.mode)
			}

			failed, err := stack.refundRepo.Get(stack.ctx, refundRow.ID)
			require.NoError(t, err)
			require.Equal(t, types.RefundStatusFailed, failed.RefundStatus)
			require.NotNil(t, failed.FailureReason, "a failed refund must record why")
			require.NotEmpty(t, *failed.FailureReason)

			// No fallback: exactly one canonical row, no wallet refund row, no
			// wallet transaction, and the invoice itself is unchanged.
			rows, err := stack.refundRepo.ListByInvoice(stack.ctx, invoiceID)
			require.NoError(t, err)
			require.Len(t, rows, 1, "a failed wechat refund must not spawn a fallback refund row")

			walletTransactions, err := stack.walletRepo.CountWalletTransactions(stack.ctx, &types.WalletTransactionFilter{
				QueryFilter: types.NewNoLimitQueryFilter(),
			})
			require.NoError(t, err)
			require.Zero(t, walletTransactions, "a failed wechat refund must not create wallet money")

			invoiceAfter := stack.mustGetInvoice(t, invoiceID)
			require.Equal(t, invoiceBefore.PaymentStatus, invoiceAfter.PaymentStatus)
			require.True(t, invoiceAfter.AmountPaid.Equal(invoiceBefore.AmountPaid),
				"amount_paid %s -> %s", invoiceBefore.AmountPaid, invoiceAfter.AmountPaid)

			// Automatic retry is refused for a failed wechat refund; assert the
			// machine-readable contract, not human wording.
			retryRouter := stack.refundRetryRouter()
			req := httptest.NewRequest(http.MethodPost, "/v1/refunds/"+refundRow.ID+"/retry", nil)
			rec := httptest.NewRecorder()
			retryRouter.ServeHTTP(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code, "retry body: %s", rec.Body.String())
			var errResp ierr.ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
			require.Equal(t, ierr.ErrCodeValidation, errResp.Code)
			require.Equal(t, http.StatusBadRequest, errResp.HTTPStatusCode)

			rowsAfter, err := stack.refundRepo.ListByInvoice(stack.ctx, invoiceID)
			require.NoError(t, err)
			require.Len(t, rowsAfter, 1, "a refused retry must not add rows either")
		})
	}
}

// refundRetryRouter mounts the production refund retry handler with the same
// error mapping middleware the real router uses.
func (s *wechatPayTestStack) refundRetryRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.ErrorHandler())
	router.POST("/v1/refunds/:id/retry", NewRefundHandler(s.refundService, s.log).RetryRefund)
	return router
}
