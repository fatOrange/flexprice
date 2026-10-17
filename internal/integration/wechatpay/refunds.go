package wechatpay

import (
	"context"
	"errors"
	"time"

	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/interfaces"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/samber/lo"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments/native"
	"github.com/wechatpay-apiv3/wechatpay-go/services/refunddomestic"
)

func (c *client) GetRefund(ctx context.Context, outRefundNo string) (*Refund, error) {
	if outRefundNo == "" {
		return nil, ierr.NewError("out_refund_no is required to query a wechatpay refund").
			WithHint("Pass the merchant refund number derived by MerchantRefundNo for the refund row").
			Mark(ierr.ErrValidation)
	}
	res, err := c.resolve(ctx)
	if err != nil {
		return nil, err
	}
	service := refunddomestic.RefundsApiService{Client: res.sdk}
	refund, _, err := service.QueryByOutRefundNo(ctx, refunddomestic.QueryByOutRefundNoRequest{
		OutRefundNo: lo.ToPtr(outRefundNo),
	})
	if err != nil {
		return nil, c.providerError(ctx, res, "query refund", err, ErrRefundNotFound, apiCodeResourceNotFound)
	}
	converted := refundFromSDK(refund)
	if converted == nil {
		return nil, missingPayloadError("refund")
	}
	return converted, nil
}

// refundPayment issues a full refund for a settled transaction. It is the
// implementation behind RefundAdapter and deliberately not part of the public
// Gateway contract; callers refund through interfaces.RefundProvider.
//
// WeChat Pay settles CNY in fen and this integration only refunds the original
// transaction amount in full: the requested amount is checked against the
// transaction WeChat Pay itself reports, never against locally believed
// amounts. The merchant refund number derives from the idempotency key, so a
// retry addresses the same refund object instead of refunding twice.
func (c *client) refundPayment(ctx context.Context, req interfaces.RefundProviderRequest) (*interfaces.RefundProviderResponse, error) {
	if req.GatewayPaymentID == "" {
		return nil, ierr.NewError("wechatpay transaction id is required to refund").
			WithHint("Pass the WeChat Pay transaction id recorded on the settled payment").
			Mark(ierr.ErrValidation)
	}
	if req.IdempotencyKey == "" {
		return nil, ierr.NewError("refund idempotency key is required").
			WithHint("Pass the FlexPrice refund row id as the idempotency key").
			Mark(ierr.ErrValidation)
	}
	outRefundNo := MerchantRefundNo(req.IdempotencyKey)
	if outRefundNo == "" {
		return nil, ierr.NewError("refund idempotency key is required").
			Mark(ierr.ErrValidation)
	}
	fen, err := amountToFen(req.Amount, req.Currency)
	if err != nil {
		return nil, err
	}

	res, err := c.resolve(ctx)
	if err != nil {
		return nil, err
	}

	transaction, err := c.transactionByID(ctx, res, req.GatewayPaymentID)
	if err != nil {
		return nil, err
	}
	if err := validateFullRefund(transaction, fen); err != nil {
		return nil, err
	}

	service := refunddomestic.RefundsApiService{Client: res.sdk}

	// A retry must not create a second refund: if the merchant refund number
	// already exists, return its current provider state instead.
	existing, _, queryErr := service.QueryByOutRefundNo(ctx, refunddomestic.QueryByOutRefundNoRequest{
		OutRefundNo: lo.ToPtr(outRefundNo),
	})
	if queryErr == nil {
		c.logger.Info(ctx, "wechatpay refund already exists; returning its current state",
			"out_refund_no", outRefundNo,
			"transaction_id", req.GatewayPaymentID)
		return refundProviderResponse(refundFromSDK(existing)), nil
	}
	if mapped := c.providerError(ctx, res, "query refund before create", queryErr, ErrRefundNotFound, apiCodeResourceNotFound); !errors.Is(mapped, ErrRefundNotFound) {
		return nil, mapped
	}

	refund, _, err := service.Create(ctx, refunddomestic.CreateRequest{
		TransactionId: lo.ToPtr(req.GatewayPaymentID),
		OutRefundNo:   lo.ToPtr(outRefundNo),
		NotifyUrl:     lo.ToPtr(res.config.notifyURL),
		Amount: &refunddomestic.AmountReq{
			Total:    lo.ToPtr(transaction.Amount.Total),
			Refund:   lo.ToPtr(fen),
			Currency: lo.ToPtr(currencyCNY),
		},
	})
	if err != nil {
		return nil, c.providerError(ctx, res, "create refund", err, nil, "")
	}
	c.logger.Info(ctx, "created wechatpay refund",
		"out_refund_no", outRefundNo,
		"transaction_id", req.GatewayPaymentID,
		"refund_fen", fen)
	return refundProviderResponse(refundFromSDK(refund)), nil
}

// validateFullRefund enforces the provider-side full refund rule: the refund
// must equal the total WeChat Pay reports for a successful CNY transaction.
func validateFullRefund(transaction *Transaction, fen int64) error {
	switch {
	case transaction == nil:
		return ierr.NewError("wechatpay transaction could not be read").
			WithHint("WeChat Pay returned no transaction for the refund target").
			Mark(ierr.ErrInternal)
	case transaction.TradeState != TradeStateSuccess:
		return ierr.NewError("wechatpay transaction is not successful").
			WithHint("Only a successful transaction can be refunded").
			WithReportableDetails(map[string]any{
				"transaction_id": transaction.TransactionID,
				"trade_state":    transaction.TradeState,
			}).
			Mark(ierr.ErrValidation)
	case !types.IsMatchingCurrency(transaction.Amount.Currency, currencyCNY):
		return ierr.NewError("wechatpay transaction is not in CNY").
			WithHint("WeChat Pay refunds are issued in the transaction currency only").
			WithReportableDetails(map[string]any{"currency": transaction.Amount.Currency}).
			Mark(ierr.ErrValidation)
	case transaction.Amount.Total != fen:
		return ierr.NewError("wechatpay refunds must equal the original transaction amount").
			WithHint("Partial refunds are not supported by this integration; refund the full transaction amount").
			WithReportableDetails(map[string]any{
				"transaction_total_fen": transaction.Amount.Total,
				"refund_fen":            fen,
			}).
			Mark(ierr.ErrValidation)
	}
	return nil
}

// refundProviderResponse maps a WeChat Pay refund onto the refund provider
// contract. WeChat Pay accepts refund requests asynchronously, so PROCESSING
// is the normal create outcome; only a SUCCESS status settles.
func refundProviderResponse(refund *Refund) *interfaces.RefundProviderResponse {
	response := &interfaces.RefundProviderResponse{
		Status:   types.RefundStatusProcessing,
		Metadata: map[string]interface{}{},
	}
	if refund == nil {
		return response
	}
	response.GatewayRefundID = refund.RefundID
	response.Metadata["out_refund_no"] = refund.OutRefundNo
	response.Metadata["transaction_id"] = refund.TransactionID
	response.Metadata["status"] = refund.RefundStatus
	switch refund.RefundStatus {
	case RefundStatusSuccess:
		response.Status = types.RefundStatusSucceeded
		response.SettledAmount = fenToAmount(refund.Amount.Refund)
	case RefundStatusClosed:
		response.Status = types.RefundStatusCancelled
	case RefundStatusAbnormal:
		// ABNORMAL needs manual handling at the merchant platform before it can
		// settle; until then it is not money returned.
		response.Status = types.RefundStatusFailed
	default:
		response.Status = types.RefundStatusProcessing
	}
	return response
}

// refundFromSDK maps the SDK's refund model onto the wire type.
func refundFromSDK(refund *refunddomestic.Refund) *Refund {
	if refund == nil {
		return nil
	}
	out := &Refund{}
	if refund.OutTradeNo != nil {
		out.OutTradeNo = *refund.OutTradeNo
	}
	if refund.TransactionId != nil {
		out.TransactionID = *refund.TransactionId
	}
	if refund.OutRefundNo != nil {
		out.OutRefundNo = *refund.OutRefundNo
	}
	if refund.RefundId != nil {
		out.RefundID = *refund.RefundId
	}
	if refund.Status != nil {
		out.RefundStatus = string(*refund.Status)
	}
	if refund.SuccessTime != nil {
		out.SuccessTime = refund.SuccessTime.Format(time.RFC3339)
	}
	if refund.Amount != nil {
		if refund.Amount.Total != nil {
			out.Amount.Total = *refund.Amount.Total
		}
		if refund.Amount.Refund != nil {
			out.Amount.Refund = *refund.Amount.Refund
		}
		if refund.Amount.Currency != nil {
			out.Amount.Currency = *refund.Amount.Currency
		}
	}
	return out
}

// transactionByID queries a transaction with an already-resolved SDK client,
// so multi-step flows resolve the tenant connection once.
func (c *client) transactionByID(ctx context.Context, res *resolvedConfig, transactionID string) (*Transaction, error) {
	service := native.NativeApiService{Client: res.sdk}
	tx, _, err := service.QueryOrderById(ctx, native.QueryOrderByIdRequest{
		TransactionId: lo.ToPtr(transactionID),
		Mchid:         lo.ToPtr(res.config.merchantID),
	})
	if err != nil {
		return nil, c.providerError(ctx, res, "query order by transaction_id", err, ErrOrderNotFound, apiCodeOrderNotFound)
	}
	converted := transactionFromSDK(tx)
	if converted == nil {
		return nil, missingPayloadError("transaction")
	}
	return converted, nil
}
