package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/flexprice/flexprice/internal/api/dto"
	"github.com/flexprice/flexprice/internal/domain/payment"
	"github.com/flexprice/flexprice/internal/domain/refund"
	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/integration/wechatpay"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/samber/lo"
	"github.com/shopspring/decimal"
)

// wechatPayRefundKey is the idempotency key that was handed to the refund
// adapter; the merchant refund number is derived from it deterministically, so
// the same key is what the provider's refund is queried back with.
func wechatPayRefundKey(row *refund.Refund) string {
	return lo.FromPtrOr(row.GatewayIdempotencyToken, row.IdempotencyKey)
}

// validateWechatPayFullRefund rejects a partial WeChat Pay refund while refund
// rows are still being planned. WeChat Pay returns money only as the full
// original transaction, and the rejection happens before anything is persisted,
// so a refused request cannot leave reserved refund rows or a credit note
// behind. Wallet-bound refunds are not gateway refunds and are unaffected.
func validateWechatPayFullRefund(p *payment.Payment, allocated decimal.Decimal, allowGateway bool) error {
	if !allowGateway || p == nil || p.PaymentGateway == nil {
		return nil
	}
	if types.PaymentGatewayType(*p.PaymentGateway) != types.PaymentGatewayTypeWechatPay {
		return nil
	}
	if allocated.Equal(p.Amount) {
		return nil
	}

	return ierr.NewError("wechatpay refunds must return the full payment amount").
		WithHintf(
			"WeChat Pay can only return the full original transaction of %s %s; a partial refund to the original source is not supported.",
			p.Amount, p.Currency,
		).
		WithReportableDetails(map[string]any{
			"payment_id":     p.ID,
			"payment_amount": p.Amount,
			"refund_amount":  allocated,
			"currency":       p.Currency,
		}).
		Mark(ierr.ErrValidation)
}

// syncRefundStatusFromGateway reconciles one in-flight gateway refund against
// the provider. It is best effort by contract: the caller treats an error as
// "status unknown" and keeps the canonical row untouched.
func (s *refundService) syncRefundStatusFromGateway(ctx context.Context, row *refund.Refund) (*refund.Refund, error) {
	if row == nil ||
		row.RefundStatus != types.RefundStatusProcessing ||
		row.RefundDestination != types.RefundDestinationGateway ||
		row.PaymentGateway == nil ||
		types.PaymentGatewayType(*row.PaymentGateway) != types.PaymentGatewayTypeWechatPay {
		return row, nil
	}
	return reconcileWechatPayRefund(ctx, s.ServiceParams, row)
}

// reconcileWechatPayRefund queries a pending refund at WeChat Pay and settles or
// fails the canonical row from the verified result. It shares the refund amount
// validation with the callback path, and the refund transition guards make a
// redelivered or re-queried outcome settle at most once.
func reconcileWechatPayRefund(ctx context.Context, params ServiceParams, row *refund.Refund) (*refund.Refund, error) {
	client, err := params.IntegrationFactory.GetWechatPayClient(ctx)
	if err != nil {
		return row, err
	}

	providerRefund, err := client.GetRefund(ctx, wechatpay.MerchantRefundNo(wechatPayRefundKey(row)))
	if err != nil {
		// A missing or unreadable refund at the provider is an unconfirmed
		// outcome, not a failure: the canonical row keeps its PROCESSING claim
		// and capacity reservation until a verified status says otherwise. No
		// replacement refund is started from a query gap.
		return row, err
	}

	refundSvc := NewRefundService(params)
	switch string(providerRefund.RefundStatus) {
	case string(wechatpay.RefundStatusSuccess):
		if err := validateWechatPayRefundAmount(providerRefund, row.Currency, row.Amount); err != nil {
			return row, err
		}
		if err := refundSvc.Settle(ctx, &dto.SettleRefundRequest{
			RefundID:      row.ID,
			SettledAmount: row.Amount,
			DestinationID: lo.ToPtr(providerRefund.RefundID),
			GatewayMetadata: map[string]interface{}{
				"status":              providerRefund.RefundStatus,
				"out_refund_no":       providerRefund.OutRefundNo,
				"wechatpay_refund_id": providerRefund.RefundID,
				"reconciled_by":       "query",
			},
		}); err != nil {
			return row, err
		}
	case string(wechatpay.RefundStatusAbnormal), string(wechatpay.RefundStatusClosed):
		if err := refundSvc.Fail(ctx, row.ID,
			fmt.Sprintf("wechatpay reported the refund as %s", providerRefund.RefundStatus)); err != nil {
			return row, err
		}
	default:
		// Still processing at the provider; nothing to record yet.
		return row, nil
	}

	return params.RefundRepo.Get(ctx, row.ID)
}

// validateWechatPayRefundAmount checks the provider's refund amount and currency
// against the canonical planned amount, in fen.
func validateWechatPayRefundAmount(providerRefund *wechatpay.Refund, currency string, amount decimal.Decimal) error {
	if providerRefund == nil {
		return ierr.NewError("wechatpay refund payload is missing").
			WithHint("A refund notification or query result must carry its payload").
			Mark(ierr.ErrValidation)
	}
	if !strings.EqualFold(providerRefund.Amount.Currency, "CNY") || !strings.EqualFold(currency, "CNY") {
		return ierr.NewError("wechatpay refund is not in CNY").
			WithHint("Only CNY refunds are supported").
			WithReportableDetails(map[string]any{
				"provider_currency": providerRefund.Amount.Currency,
				"refund_currency":   currency,
			}).
			Mark(ierr.ErrValidation)
	}
	wantFen, err := wechatPayCents(amount)
	if err != nil {
		return err
	}
	if providerRefund.Amount.Refund != wantFen {
		return ierr.NewError("wechatpay refund amount does not match the planned refund").
			WithHint("The amount the provider will return differs from the canonical refund amount").
			WithReportableDetails(map[string]any{
				"provider_refund_fen": providerRefund.Amount.Refund,
				"planned_refund_fen":  wantFen,
			}).
			Mark(ierr.ErrValidation)
	}
	return nil
}
