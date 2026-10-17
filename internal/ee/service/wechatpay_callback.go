package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/flexprice/flexprice/internal/api/dto"
	"github.com/flexprice/flexprice/internal/domain/refund"
	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/integration/wechatpay"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/samber/lo"
)

// HandleCallback verifies one raw signed WeChat Pay callback and applies its
// business effect. Nothing here trusts the raw body: signature verification and
// decryption happen inside the gateway client, and every field the effect relies
// on is then checked against the local payment and the tenant's connection.
//
// The returned error is the ACK contract: nil means the effect is durably
// recorded and the provider may stop retrying; any error means the provider
// should deliver the notification again.
func (s *wechatPayService) HandleCallback(ctx context.Context, headers http.Header, rawBody []byte) error {
	client, err := s.IntegrationFactory.GetWechatPayClient(ctx)
	if err != nil {
		return err
	}

	notification, err := client.ParseNotification(ctx, headers, rawBody)
	if err != nil {
		// The HTTP boundary logs this rejection once with the tenant and
		// environment locators; the service only returns the error.
		return err
	}
	if notification == nil {
		return ierr.NewError("wechatpay callback verified to an empty notification").
			WithHint("The notification carried neither a transaction nor a refund").
			Mark(ierr.ErrValidation)
	}

	switch string(notification.EventType) {
	case string(wechatpay.EventTypeTransactionSuccess):
		if notification.Transaction == nil {
			return ierr.NewError("wechatpay transaction notification has no transaction").
				WithHint("A TRANSACTION.SUCCESS notification must carry its transaction").
				Mark(ierr.ErrValidation)
		}
		p, err := s.paymentByOutTradeNo(ctx, notification.Transaction.OutTradeNo)
		if err != nil {
			return err
		}
		return s.settleTransaction(ctx, p, notification.Transaction)

	case string(wechatpay.EventTypeRefundSuccess):
		return s.settleRefundNotification(ctx, notification.Refund)

	case string(wechatpay.EventTypeRefundAbnormal), string(wechatpay.EventTypeRefundClosed):
		return s.failRefundNotification(ctx, notification.Refund)

	default:
		// Unknown or non-mutating events (including TRANSACTION.CLOSED, which the
		// query path resolves with query-before-close) are accepted without
		// touching state; there is nothing to record and nothing to retry.
		s.Logger.Info(ctx, "unhandled wechatpay callback event accepted",
			"event_type", notification.EventType)
		return nil
	}
}

// settleRefundNotification settles the canonical refund row a verified
// REFUND.SUCCESS notification points at. Redeliveries are absorbed by the refund
// transition guard, so a refund can only be recorded once.
func (s *wechatPayService) settleRefundNotification(ctx context.Context, providerRefund *wechatpay.Refund) error {
	if providerRefund == nil {
		return ierr.NewError("wechatpay refund notification has no refund").
			WithHint("A refund notification must carry its refund payload").
			Mark(ierr.ErrValidation)
	}
	row, err := s.refundRowFor(ctx, providerRefund)
	if err != nil {
		return err
	}
	if err := s.validateRefundNotification(ctx, row, providerRefund); err != nil {
		return err
	}

	return NewRefundService(s.ServiceParams).Settle(ctx, &dto.SettleRefundRequest{
		RefundID:      row.ID,
		SettledAmount: row.Amount,
		DestinationID: lo.ToPtr(providerRefund.RefundID),
		GatewayMetadata: map[string]interface{}{
			"status":              providerRefund.RefundStatus,
			"out_refund_no":       providerRefund.OutRefundNo,
			"wechatpay_refund_id": providerRefund.RefundID,
		},
	})
}

// failRefundNotification records a verified refund the provider will never pay
// out. Fail routes through the canonical refund service, which owns what a
// failed gateway refund means for the customer's money.
func (s *wechatPayService) failRefundNotification(ctx context.Context, providerRefund *wechatpay.Refund) error {
	if providerRefund == nil {
		return ierr.NewError("wechatpay refund notification has no refund").
			WithHint("A refund notification must carry its refund payload").
			Mark(ierr.ErrValidation)
	}
	row, err := s.refundRowFor(ctx, providerRefund)
	if err != nil {
		return err
	}
	if err := s.validateRefundNotification(ctx, row, providerRefund); err != nil {
		return err
	}

	return NewRefundService(s.ServiceParams).Fail(ctx, row.ID,
		fmt.Sprintf("wechatpay reported the refund as %s", providerRefund.RefundStatus))
}

// refundRowFor resolves the canonical refund row a notification refers to: first
// by the provider's refund id, then by the merchant refund number derived from
// the row's idempotency key for refunds whose creation response never carried an
// id.
func (s *wechatPayService) refundRowFor(ctx context.Context, providerRefund *wechatpay.Refund) (*refund.Refund, error) {
	if providerRefund.RefundID != "" {
		row, err := NewRefundService(s.ServiceParams).GetRefundByGatewayRefundID(
			ctx, string(types.PaymentGatewayTypeWechatPay), providerRefund.RefundID)
		if err == nil && row != nil {
			return row.Refund, nil
		}
		if err != nil && !ierr.IsNotFound(err) {
			return nil, err
		}
	}

	if providerRefund.OutTradeNo != "" && providerRefund.OutRefundNo != "" {
		p, err := s.paymentByOutTradeNo(ctx, providerRefund.OutTradeNo)
		if err != nil {
			return nil, err
		}
		rows, err := s.RefundRepo.ListByInvoice(ctx, p.DestinationID)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row == nil || row.PaymentID == nil || *row.PaymentID != p.ID {
				continue
			}
			if wechatpay.MerchantRefundNo(wechatPayRefundKey(row)) == providerRefund.OutRefundNo {
				return row, nil
			}
		}
	}

	return nil, ierr.NewError("no refund matches this wechatpay notification").
		WithHint("The notification references a refund this environment never planned").
		WithReportableDetails(map[string]any{
			"wechatpay_refund_id": providerRefund.RefundID,
			"out_refund_no":       providerRefund.OutRefundNo,
			"out_trade_no":        providerRefund.OutTradeNo,
		}).
		Mark(ierr.ErrNotFound)
}

// validateRefundNotification checks a verified refund against the canonical row:
// amount, currency, and the original transaction it draws from must all match
// before the ledger records anything.
func (s *wechatPayService) validateRefundNotification(ctx context.Context, row *refund.Refund, providerRefund *wechatpay.Refund) error {
	if err := validateWechatPayRefundAmount(providerRefund, row.Currency, row.Amount); err != nil {
		return ierr.WithError(err).
			WithHint("The refund amount or currency does not match the canonical refund").
			WithReportableDetails(map[string]any{"refund_id": row.ID}).
			Mark(ierr.ErrValidation)
	}
	if row.PaymentID == nil {
		return ierr.NewError("wechatpay refund has no source payment").
			WithHint("A gateway refund must be linked to the payment it draws from").
			WithReportableDetails(map[string]any{"refund_id": row.ID}).
			Mark(ierr.ErrValidation)
	}

	p, err := s.PaymentRepo.Get(ctx, *row.PaymentID)
	if err != nil {
		return err
	}
	wantTotal, err := wechatPayCents(p.Amount)
	if err != nil {
		return err
	}
	if providerRefund.Amount.Total != wantTotal {
		return ierr.NewError("wechatpay refund total does not match the original transaction").
			WithHint("WeChat Pay only refunds the full original transaction; the notification's total differs from the payment").
			WithReportableDetails(map[string]any{
				"refund_id":         row.ID,
				"payment_id":        p.ID,
				"refund_total_fen":  providerRefund.Amount.Total,
				"payment_total_fen": wantTotal,
			}).
			Mark(ierr.ErrValidation)
	}
	if bound := lo.FromPtr(p.GatewayTrackingID); bound != "" && providerRefund.OutTradeNo != "" && bound != providerRefund.OutTradeNo {
		return ierr.NewError("wechatpay refund does not match the bound order").
			WithHint("The refund's merchant order number differs from the one bound to the payment").
			WithReportableDetails(map[string]any{
				"refund_id":       row.ID,
				"bound_order_id":  bound,
				"refund_order_id": providerRefund.OutTradeNo,
			}).
			Mark(ierr.ErrValidation)
	}
	if !strings.EqualFold(row.Currency, "CNY") {
		return ierr.NewError("wechatpay refund is not in CNY").
			WithHint("Only CNY refunds are supported").
			WithReportableDetails(map[string]any{"refund_id": row.ID, "currency": row.Currency}).
			Mark(ierr.ErrValidation)
	}
	return nil
}
