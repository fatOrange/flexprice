package wechatpay

import (
	"context"
	"errors"
	"time"

	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/interfaces"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/samber/lo"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments/jsapi"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments/native"
)

// maxDescriptionLength is WeChat Pay's limit for the order goods description.
const maxDescriptionLength = 127

// chinaStandardTime is the UTC+8 zone WeChat Pay documents for time_expire.
// The provider accepts RFC3339 offsets; this is the one its own examples use.
var chinaStandardTime = time.FixedZone("CST", 8*60*60)

func (c *client) CreatePayment(
	ctx context.Context,
	req interfaces.CheckoutProviderRequest,
	options types.WechatPayOptions,
) (*interfaces.CheckoutProviderResponse, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	orderID := MerchantOrderID(req.PaymentID)
	if orderID == "" {
		return nil, ierr.NewError("payment id is required to create a wechatpay payment").
			WithHint("A persisted FlexPrice payment id is required; the WeChat Pay merchant order number derives from it").
			Mark(ierr.ErrValidation)
	}
	fen, err := amountToFen(req.Amount, req.Currency)
	if err != nil {
		return nil, err
	}
	expiresAt, err := paymentExpire(req.ExpiresAt, time.Now())
	if err != nil {
		return nil, err
	}

	res, err := c.resolve(ctx)
	if err != nil {
		return nil, err
	}
	description := paymentDescription(req)
	timeExpire := expiresAt.In(chinaStandardTime)

	switch options.EffectivePaymentType() {
	case types.WechatPayPaymentTypeNative:
		service := native.NativeApiService{Client: res.sdk}
		resp, _, err := service.Prepay(ctx, native.PrepayRequest{
			Appid:       lo.ToPtr(res.config.appID),
			Mchid:       lo.ToPtr(res.config.merchantID),
			Description: lo.ToPtr(description),
			OutTradeNo:  lo.ToPtr(orderID),
			TimeExpire:  lo.ToPtr(timeExpire),
			NotifyUrl:   lo.ToPtr(res.config.notifyURL),
			Amount: &native.Amount{
				Total:    lo.ToPtr(fen),
				Currency: lo.ToPtr(currencyCNY),
			},
		})
		if err != nil {
			return nil, c.providerError(ctx, res, "prepay native", err, nil, "")
		}
		if resp == nil || resp.CodeUrl == nil || *resp.CodeUrl == "" {
			return nil, missingPayloadError("native prepay response code_url")
		}
		c.logger.Info(ctx, "created wechatpay native payment",
			"out_trade_no", orderID,
			"payment_id", req.PaymentID,
			"invoice_id", req.InvoiceID,
			"expires_at", expiresAt)
		return &interfaces.CheckoutProviderResponse{
			ProviderSessionID: orderID,
			NextAction: types.PaymentAction{
				Type:      types.PaymentActionTypeQRCode,
				QRContent: *resp.CodeUrl,
			},
			ExpiresAt:        lo.ToPtr(expiresAt),
			ProviderMetadata: map[string]string{"out_trade_no": orderID},
		}, nil

	case types.WechatPayPaymentTypeJSAPI:
		service := jsapi.JsapiApiService{Client: res.sdk}
		resp, _, err := service.Prepay(ctx, jsapi.PrepayRequest{
			Appid:       lo.ToPtr(res.config.appID),
			Mchid:       lo.ToPtr(res.config.merchantID),
			Description: lo.ToPtr(description),
			OutTradeNo:  lo.ToPtr(orderID),
			TimeExpire:  lo.ToPtr(timeExpire),
			NotifyUrl:   lo.ToPtr(res.config.notifyURL),
			Amount: &jsapi.Amount{
				Total:    lo.ToPtr(fen),
				Currency: lo.ToPtr(currencyCNY),
			},
			Payer: &jsapi.Payer{Openid: lo.ToPtr(options.OpenID)},
		})
		if err != nil {
			return nil, c.providerError(ctx, res, "prepay jsapi", err, nil, "")
		}
		if resp == nil || resp.PrepayId == nil || *resp.PrepayId == "" {
			return nil, missingPayloadError("jsapi prepay response prepay_id")
		}
		params, err := buildJSAPIParameters(res.config.appID, *resp.PrepayId, res.config.merchantPrivateKey, time.Now())
		if err != nil {
			return nil, err
		}
		c.logger.Info(ctx, "created wechatpay jsapi payment",
			"out_trade_no", orderID,
			"payment_id", req.PaymentID,
			"invoice_id", req.InvoiceID,
			"expires_at", expiresAt)
		return &interfaces.CheckoutProviderResponse{
			ProviderSessionID: orderID,
			NextAction: types.PaymentAction{
				Type:  types.PaymentActionTypeJSAPI,
				JSAPI: params,
			},
			ExpiresAt: lo.ToPtr(expiresAt),
			ProviderMetadata: map[string]string{
				"out_trade_no": orderID,
				"prepay_id":    *resp.PrepayId,
			},
		}, nil

	default:
		// options.Validate rejects every other channel, so this is a guard
		// rather than a reachable path.
		return nil, ierr.NewError("unsupported wechatpay payment_type").
			WithHint("Allowed values: native, jsapi").
			WithReportableDetails(map[string]any{"payment_type": options.PaymentType}).
			Mark(ierr.ErrValidation)
	}
}

func (c *client) GetTransaction(ctx context.Context, orderID string) (*Transaction, error) {
	if orderID == "" {
		return nil, ierr.NewError("out_trade_no is required to query a wechatpay transaction").
			WithHint("Pass the merchant order number bound to the payment attempt").
			Mark(ierr.ErrValidation)
	}
	res, err := c.resolve(ctx)
	if err != nil {
		return nil, err
	}
	service := native.NativeApiService{Client: res.sdk}
	tx, _, err := service.QueryOrderByOutTradeNo(ctx, native.QueryOrderByOutTradeNoRequest{
		OutTradeNo: lo.ToPtr(orderID),
		Mchid:      lo.ToPtr(res.config.merchantID),
	})
	if err != nil {
		return nil, c.providerError(ctx, res, "query order by out_trade_no", err, ErrOrderNotFound, apiCodeOrderNotFound)
	}
	converted := transactionFromSDK(tx)
	if converted == nil {
		return nil, missingPayloadError("transaction")
	}
	return converted, nil
}

func (c *client) GetTransactionByID(ctx context.Context, transactionID string) (*Transaction, error) {
	if transactionID == "" {
		return nil, ierr.NewError("transaction_id is required to query a wechatpay transaction").
			WithHint("Pass the WeChat Pay transaction id recorded on the settled payment").
			Mark(ierr.ErrValidation)
	}
	res, err := c.resolve(ctx)
	if err != nil {
		return nil, err
	}
	return c.transactionByID(ctx, res, transactionID)
}

func (c *client) CloseOrder(ctx context.Context, orderID string) error {
	if orderID == "" {
		return ierr.NewError("out_trade_no is required to close a wechatpay order").
			WithHint("Pass the merchant order number bound to the payment attempt").
			Mark(ierr.ErrValidation)
	}
	res, err := c.resolve(ctx)
	if err != nil {
		return err
	}
	service := native.NativeApiService{Client: res.sdk}
	_, err = service.CloseOrder(ctx, native.CloseOrderRequest{
		OutTradeNo: lo.ToPtr(orderID),
		Mchid:      lo.ToPtr(res.config.merchantID),
	})
	if err != nil {
		mapped := c.providerError(ctx, res, "close order", err, ErrOrderNotFound, apiCodeOrderNotFound)
		if errors.Is(mapped, errOrderClosed) {
			// Reported only by a verified response: the desired end state.
			c.logger.Info(ctx, "wechatpay order was already closed", "out_trade_no", orderID)
			return nil
		}
		return mapped
	}
	c.logger.Info(ctx, "closed wechatpay order", "out_trade_no", orderID)
	return nil
}

// paymentExpire resolves the provider time_expire for a checkout attempt. The
// expiry is fixed when the attempt is first created and persisted by the
// caller; retries must never reset it, so nil only fills in the default link
// expiry for a caller that did not persist one. A persisted expiry that has
// already passed is rejected: the attempt is dead and WeChat Pay would refuse
// the order anyway.
func paymentExpire(requested *time.Time, now time.Time) (time.Time, error) {
	if requested == nil || requested.IsZero() {
		return now.Add(types.CheckoutPaymentProviderWechatPay.LinkExpiry()).Truncate(time.Second), nil
	}
	expiresAt := requested.Truncate(time.Second)
	if !expiresAt.After(now) {
		return time.Time{}, ierr.NewError("wechatpay checkout attempt has expired").
			WithHint("The persisted attempt expiry has passed; start a new payment attempt instead of retrying this one").
			WithReportableDetails(map[string]any{"expires_at": expiresAt.UTC().Format(time.RFC3339)}).
			Mark(ierr.ErrValidation)
	}
	return expiresAt, nil
}

// paymentDescription is the goods description WeChat Pay requires. The invoice
// is the unit of payment here, so the invoice names the order and the payment
// id is the fallback. The payment id is never empty at this point.
func paymentDescription(req interfaces.CheckoutProviderRequest) string {
	if req.InvoiceID != "" {
		return truncateDescription("Invoice " + req.InvoiceID)
	}
	return truncateDescription("Payment " + req.PaymentID)
}

// truncateDescription clips the description to WeChat Pay's character limit.
func truncateDescription(description string) string {
	runes := []rune(description)
	if len(runes) <= maxDescriptionLength {
		return description
	}
	return string(runes[:maxDescriptionLength])
}

// missingPayloadError reports a successful provider response that lacks the
// payload the caller needs to continue.
func missingPayloadError(payload string) error {
	return ierr.NewError("wechatpay response is missing required data").
		WithHintf("WeChat Pay returned success without %s", payload).
		Mark(ierr.ErrInternal)
}

// transactionFromSDK maps the SDK's transaction model onto the wire type.
func transactionFromSDK(tx *payments.Transaction) *Transaction {
	if tx == nil {
		return nil
	}
	out := &Transaction{}
	if tx.Appid != nil {
		out.AppID = *tx.Appid
	}
	if tx.Mchid != nil {
		out.MerchantID = *tx.Mchid
	}
	if tx.OutTradeNo != nil {
		out.OutTradeNo = *tx.OutTradeNo
	}
	if tx.TransactionId != nil {
		out.TransactionID = *tx.TransactionId
	}
	if tx.TradeState != nil {
		out.TradeState = *tx.TradeState
	}
	if tx.SuccessTime != nil {
		out.SuccessTime = *tx.SuccessTime
	}
	if tx.Amount != nil {
		if tx.Amount.Total != nil {
			out.Amount.Total = *tx.Amount.Total
		}
		if tx.Amount.Currency != nil {
			out.Amount.Currency = *tx.Amount.Currency
		}
	}
	return out
}
