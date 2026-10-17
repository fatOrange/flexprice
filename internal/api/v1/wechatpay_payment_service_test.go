package v1

import (
	"net/http"
	"testing"

	"github.com/flexprice/flexprice/internal/api/dto"
	"github.com/flexprice/flexprice/internal/ee/service"
	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// TestWechatPayPaymentCreationReusesLiveIntentAndRejectsChannelSwitch proves the
// double-collection guard at the service seam: one invoice can only ever have a
// single open WeChat Pay order, and a same-invoice request on a different
// channel is refused instead of minting a second payable order.
func TestWechatPayPaymentCreationReusesLiveIntentAndRejectsChannelSwitch(t *testing.T) {
	stack := newWechatPayTestStack(t)
	stack.seedFinalizedInvoice(t, "inv_wp_reuse", "100.00")
	paymentService := service.NewPaymentService(stack.params)

	nativeRequest := func() *dto.CreatePaymentRequest {
		return &dto.CreatePaymentRequest{
			DestinationType:   types.PaymentDestinationTypeInvoice,
			DestinationID:     "inv_wp_reuse",
			PaymentMethodType: types.PaymentMethodTypePaymentLink,
			PaymentGateway:    new(types.PaymentGatewayTypeWechatPay),
			Amount:            decimal.RequireFromString("100.00"),
			Currency:          "CNY",
			ProcessPayment:    true,
			GatewayOptions: &dto.PaymentGatewayOptions{
				WechatPay: &types.WechatPayOptions{PaymentType: types.WechatPayPaymentTypeNative},
			},
		}
	}

	first, err := paymentService.CreatePayment(stack.ctx, nativeRequest())
	require.NoError(t, err)
	require.Equal(t, types.PaymentStatusPending, first.PaymentStatus)
	require.NotNil(t, first.NextAction)
	require.Equal(t, types.PaymentActionTypeQRCode, first.NextAction.Type)
	require.Equal(t, 1, countPrepayCreates(stack), "exactly one native order may be created")

	second, err := paymentService.CreatePayment(stack.ctx, nativeRequest())
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID, "a live intent on the same channel must be reused")
	require.Equal(t, 1, countPrepayCreates(stack), "reuse must not create a second provider order")

	jsapiRequest := nativeRequest()
	jsapiRequest.GatewayOptions = &dto.PaymentGatewayOptions{
		WechatPay: &types.WechatPayOptions{
			PaymentType: types.WechatPayPaymentTypeJSAPI,
			OpenID:      "oFakeOpenIDForReuseTest00001",
		},
	}
	_, err = paymentService.CreatePayment(stack.ctx, jsapiRequest)
	require.Error(t, err, "a live order on another channel must refuse the request")
	require.True(t, ierr.IsValidation(err), "channel switch is a validation refusal")

	rows := stack.paymentRows(t, "inv_wp_reuse")
	require.Len(t, rows, 1, "no second payment row may exist for the invoice")
	require.Equal(t, 1, countPrepayCreates(stack), "the refusal must not reach the provider")
}

// TestWechatPayPaymentCreationIdempotencyKeyReturnsExistingRow covers the
// admission-transaction fallback: once the original intent is no longer "live",
// a repeat with the same explicit idempotency key must return the existing row
// through the repository conflict path — never a second payment or provider
// order.
func TestWechatPayPaymentCreationIdempotencyKeyReturnsExistingRow(t *testing.T) {
	stack := newWechatPayTestStack(t)
	stack.seedFinalizedInvoice(t, "inv_wp_idem", "100.00")
	paymentService := service.NewPaymentService(stack.params)

	request := func() *dto.CreatePaymentRequest {
		return &dto.CreatePaymentRequest{
			IdempotencyKey:    "wp-explicit-idem-key-0001",
			DestinationType:   types.PaymentDestinationTypeInvoice,
			DestinationID:     "inv_wp_idem",
			PaymentMethodType: types.PaymentMethodTypePaymentLink,
			PaymentGateway:    new(types.PaymentGatewayTypeWechatPay),
			Amount:            decimal.RequireFromString("100.00"),
			Currency:          "CNY",
			ProcessPayment:    true,
			GatewayOptions: &dto.PaymentGatewayOptions{
				WechatPay: &types.WechatPayOptions{PaymentType: types.WechatPayPaymentTypeNative},
			},
		}
	}

	created, err := paymentService.CreatePayment(stack.ctx, request())
	require.NoError(t, err)
	require.Equal(t, types.PaymentStatusPending, created.PaymentStatus)
	require.NotNil(t, created.NextAction)
	require.Equal(t, 1, countPrepayCreates(stack))

	// Past the persisted deadline the live-intent check no longer shields the
	// row, so only the idempotency key keeps the admission idempotent.
	stack.stageExpiry(t, created.ID)
	prepaysBefore := countPrepayCreates(stack)

	existing, err := paymentService.CreatePayment(stack.ctx, request())
	require.NoError(t, err)
	require.Equal(t, created.ID, existing.ID, "an identical idempotency key must return the existing row")
	require.Equal(t, prepaysBefore, countPrepayCreates(stack), "the repeat must not create a provider order")
	require.Len(t, stack.paymentRows(t, "inv_wp_idem"), 1, "no second payment row may exist")
}

// TestWechatPayDuplicateCreateReturnsCurrentRecordAcrossStates pins the race
// fix at the public service seam: a duplicate same-channel create must return
// the CURRENT record for any live state, never re-enter the processor from a
// stale snapshot or create a second provider order. Both states are staged
// deterministically; the true concurrent interleave is proven by the real-PG
// proof runner.
func TestWechatPayDuplicateCreateReturnsCurrentRecordAcrossStates(t *testing.T) {
	newAdmittedFixture := func(t *testing.T) (*wechatPayTestStack, service.PaymentService, string, string) {
		t.Helper()
		stack := newWechatPayTestStack(t)
		const invoiceID = "inv_wp_dup_state"
		stack.seedFinalizedInvoice(t, invoiceID, "100.00")
		paymentService := service.NewPaymentService(stack.params)
		created, err := paymentService.CreatePayment(stack.ctx, wechatPayNativeCreate(invoiceID))
		require.NoError(t, err)
		require.Equal(t, types.PaymentStatusPending, created.PaymentStatus)
		require.Equal(t, 1, countPrepayCreates(stack))
		return stack, paymentService, invoiceID, created.ID
	}

	t.Run("live intent already PROCESSING", func(t *testing.T) {
		stack, paymentService, invoiceID, paymentID := newAdmittedFixture(t)
		setStoredWechatPayStatus(t, stack, paymentID, types.PaymentStatusProcessing)

		prepays := countPrepayCreates(stack)
		duplicate, err := paymentService.CreatePayment(stack.ctx, wechatPayNativeCreate(invoiceID))
		require.NoError(t, err, "a duplicate create must not re-enter the processor from a stale snapshot")
		require.Equal(t, paymentID, duplicate.ID)
		require.Equal(t, types.PaymentStatusProcessing, duplicate.PaymentStatus, "the current record is returned as-is")
		require.Equal(t, prepays, countPrepayCreates(stack), "no second provider order may be created")
		require.Len(t, stack.paymentRows(t, invoiceID), 1)
	})

	t.Run("live intent PENDING without a stored action", func(t *testing.T) {
		stack, paymentService, invoiceID, paymentID := newAdmittedFixture(t)
		clearStoredWechatPayAction(t, stack, paymentID)

		prepays := countPrepayCreates(stack)
		duplicate, err := paymentService.CreatePayment(stack.ctx, wechatPayNativeCreate(invoiceID))
		require.NoError(t, err, "creation still in flight must fall back to the current record, not an error")
		require.Equal(t, paymentID, duplicate.ID)
		require.Equal(t, types.PaymentStatusPending, duplicate.PaymentStatus)
		require.Equal(t, prepays, countPrepayCreates(stack), "no second provider order may be created")
		require.Len(t, stack.paymentRows(t, invoiceID), 1)
	})
}

func wechatPayNativeCreate(invoiceID string) *dto.CreatePaymentRequest {
	return &dto.CreatePaymentRequest{
		DestinationType:   types.PaymentDestinationTypeInvoice,
		DestinationID:     invoiceID,
		PaymentMethodType: types.PaymentMethodTypePaymentLink,
		PaymentGateway:    new(types.PaymentGatewayTypeWechatPay),
		Amount:            decimal.RequireFromString("100.00"),
		Currency:          "CNY",
		ProcessPayment:    true,
		GatewayOptions: &dto.PaymentGatewayOptions{
			WechatPay: &types.WechatPayOptions{PaymentType: types.WechatPayPaymentTypeNative},
		},
	}
}

func setStoredWechatPayStatus(t *testing.T, stack *wechatPayTestStack, paymentID string, status types.PaymentStatus) {
	t.Helper()
	row, err := stack.paymentRepo.Get(stack.ctx, paymentID)
	require.NoError(t, err)
	row.PaymentStatus = status
	require.NoError(t, stack.paymentRepo.Update(stack.ctx, row))
}

func clearStoredWechatPayAction(t *testing.T, stack *wechatPayTestStack, paymentID string) {
	t.Helper()
	row, err := stack.paymentRepo.Get(stack.ctx, paymentID)
	require.NoError(t, err)
	delete(row.GatewayMetadata, dto.GatewayMetadataWechatPayAction)
	require.NoError(t, stack.paymentRepo.Update(stack.ctx, row))
}

// countPrepayCreates counts native prepay requests the simulator received.
func countPrepayCreates(stack *wechatPayTestStack) int {
	creates := 0
	for _, recorded := range stack.sim.Requests() {
		if recorded.Method == http.MethodPost && recorded.Path == "/v3/pay/transactions/native" {
			creates++
		}
	}
	return creates
}

func (s *wechatPayTestStack) paymentRows(t *testing.T, invoiceID string) []string {
	t.Helper()
	filter := types.NewNoLimitPaymentFilter()
	filter.DestinationID = &invoiceID
	rows, err := s.paymentRepo.List(s.ctx, filter)
	require.NoError(t, err)
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}
