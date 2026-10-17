package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/flexprice/flexprice/internal/api/dto"
	"github.com/flexprice/flexprice/internal/domain/invoice"
	"github.com/flexprice/flexprice/internal/domain/payment"
	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/idempotency"
	"github.com/flexprice/flexprice/internal/integration/payments"
	"github.com/flexprice/flexprice/internal/integration/wechatpay"
	"github.com/flexprice/flexprice/internal/interfaces"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/samber/lo"
	"github.com/shopspring/decimal"
)

// errWechatPaySettledConcurrently reports that another writer settled the local
// payment between the read and a conditional write. The caller stops without
// touching the gateway: the settled state wins.
var errWechatPaySettledConcurrently = errors.New("wechatpay payment settled concurrently")

// wechatPaySettlingContextKey marks a context that is already inside a WeChat
// Pay settlement. PaymentLifecycle reads the payment through the public
// service, whose best-effort gateway sync would otherwise re-enter this
// reconciler; the marker makes that read passive.
type wechatPaySettlingContextKey struct{}

func withWechatPaySettling(ctx context.Context) context.Context {
	return context.WithValue(ctx, wechatPaySettlingContextKey{}, true)
}

func isWechatPaySettling(ctx context.Context) bool {
	return ctx.Value(wechatPaySettlingContextKey{}) != nil
}

// WechatPayService applies verified WeChat Pay callbacks and reconciles local
// payments against the merchant's orders. It is the single settlement path for
// both the signed callback and the status query, so a payment can never be
// settled twice by two different code paths.
type WechatPayService interface {
	// HandleCallback verifies one raw signed callback body and applies its
	// business effect. It returns nil only once the effect is durably recorded.
	HandleCallback(ctx context.Context, headers http.Header, rawBody []byte) error

	// QueryPayment reconciles one local payment against the gateway and returns
	// the payment as it stands after reconciliation.
	QueryPayment(ctx context.Context, paymentID string) (*dto.PaymentResponse, error)
}

type wechatPayService struct {
	ServiceParams
}

// NewWechatPayService returns the WeChat Pay service for the given service params.
func NewWechatPayService(params ServiceParams) WechatPayService {
	return &wechatPayService{ServiceParams: params}
}

func newWechatPayService(params ServiceParams) *wechatPayService {
	return &wechatPayService{ServiceParams: params}
}

// createOrderForPayment creates the WeChat Pay order backing one local payment.
// The deterministic order handle and the fixed deadline are persisted before any
// network call, so a creation response that is lost (timeout) is still
// recoverable by query, and a callback that lands early can resolve the payment.
func (s *wechatPayService) createOrderForPayment(ctx context.Context, p *payment.Payment, _ *invoice.Invoice) error {
	if p == nil {
		return ierr.NewError("payment is required").
			WithHint("A WeChat Pay order needs a payment to back it").
			Mark(ierr.ErrValidation)
	}

	options, rawOptions, err := wechatPayOptionsFor(p)
	if err != nil {
		return err
	}

	// Re-read the invoice at network time: between the payment row being created
	// and this call the outstanding amount may have changed, and WeChat Pay
	// orders must always cover exactly what is still owed.
	invoiceObj, err := s.InvoiceRepo.Get(ctx, p.DestinationID)
	if err != nil {
		return err
	}
	if err := validateWechatPayInvoicePayment(invoiceObj, p.Amount, p.Currency); err != nil {
		return err
	}

	if boundOrderID := lo.FromPtr(p.GatewayTrackingID); boundOrderID != "" {
		// This intent is already bound to an order. Ask the provider first: the
		// first attempt may have timed out after the request reached WeChat, or
		// may never have reached it at all. The deadline is never extended.
		client, err := s.IntegrationFactory.GetWechatPayClient(ctx)
		if err != nil {
			return err
		}
		// Only the query outcome is needed here: when the order exists the
		// reconciler re-reads it and applies the verified state, so the raw
		// transaction is deliberately discarded.
		_, queryErr := client.GetTransaction(ctx, boundOrderID)
		switch {
		case queryErr == nil:
			// The order exists; reuse the reconciler so the outcome (settle,
			// expire, or await payment) is applied exactly once.
			_, err := s.reconcilePayment(ctx, p)
			return err
		case errors.Is(queryErr, wechatpay.ErrOrderNotFound):
			// The provider never saw the first request. If the fixed deadline has
			// passed, the intent is dead; otherwise create the same order now —
			// the out_trade_no is the persisted handle, so a duplicate can never
			// be minted.
			if !time.Now().UTC().Before(wechatPayExpiry(p)) {
				_, err := s.markExpired(ctx, p, "wechatpay order expired before it was created")
				return err
			}
		default:
			// Unknown provider state: never create and never expire on ambiguity.
			return queryErr
		}
	}

	orderID := wechatpay.MerchantOrderID(p.ID)
	if bound := lo.FromPtr(p.GatewayTrackingID); bound != "" {
		orderID = bound
	}
	expiresAt := wechatPayExpiry(p)
	if err := s.bindOrder(ctx, p, orderID, expiresAt, rawOptions); err != nil {
		if errors.Is(err, errWechatPaySettledConcurrently) {
			// A callback settled the payment before any order was created; there
			// is nothing left to charge.
			return nil
		}
		return err
	}

	provider, err := s.IntegrationFactory.GetCheckoutProvider(
		ctx,
		types.CheckoutPaymentProviderWechatPay,
		NewCustomerService(s.ServiceParams),
		NewInvoiceService(s.ServiceParams),
	)
	if err != nil {
		return err
	}

	resp, err := provider.CreatePaymentLink(ctx, interfaces.CheckoutProviderRequest{
		InvoiceID:        p.DestinationID,
		CustomerID:       invoiceObj.CustomerID,
		Amount:           p.Amount,
		Currency:         p.Currency,
		PaymentID:        p.ID,
		ExpiresAt:        &expiresAt,
		Metadata:         linkMetadataFor(p),
		WechatPayOptions: options,
	})
	if err != nil {
		// The request may have reached WeChat before failing (a timeout): the
		// persisted handle is what a later query reconciles against, so the error
		// is surfaced without re-attempting the order here.
		s.Logger.Error(ctx, "failed to create wechatpay order",
			"error", err,
			"payment_id", p.ID,
			"invoice_id", p.DestinationID)
		return err
	}
	if resp == nil || resp.NextAction.Type == "" {
		return ierr.NewError("wechatpay order was created without a customer action").
			WithHint("The gateway response carried no payable action; the order can still be reconciled by query").
			WithReportableDetails(map[string]any{"payment_id": p.ID}).
			Mark(ierr.ErrSystem)
	}

	return s.persistCreatedOrder(ctx, p, resp)
}

// bindOrder persists the order handle and the fixed deadline before the network
// call, conditioned on the status read earlier so a concurrent settlement is
// never overwritten.
func (s *wechatPayService) bindOrder(ctx context.Context, p *payment.Payment, orderID string, expiresAt time.Time, rawOptions string) error {
	applyWechatPayBinding(p, orderID, expiresAt, rawOptions)
	err := s.PaymentRepo.UpdateWithExpectedStatus(ctx, p, p.PaymentStatus)
	if err == nil {
		return nil
	}
	if !ierr.IsVersionConflict(err) {
		return err
	}

	latest, getErr := s.PaymentRepo.Get(ctx, p.ID)
	if getErr != nil {
		return getErr
	}
	if latest.PaymentStatus == types.PaymentStatusSucceeded || latest.PaymentStatus.IsTerminal() {
		return errWechatPaySettledConcurrently
	}

	// The row changed for another reason; adopt the newer copy and try once more.
	*p = *latest
	applyWechatPayBinding(p, orderID, expiresAt, rawOptions)
	return s.PaymentRepo.UpdateWithExpectedStatus(ctx, p, p.PaymentStatus)
}

func applyWechatPayBinding(p *payment.Payment, orderID string, expiresAt time.Time, rawOptions string) {
	p.GatewayTrackingID = lo.ToPtr(orderID)
	if p.GatewayMetadata == nil {
		p.GatewayMetadata = types.Metadata{}
	}
	p.GatewayMetadata[dto.GatewayMetadataWechatPayExpiresAt] = expiresAt.UTC().Format(time.RFC3339)
	if rawOptions != "" {
		p.GatewayMetadata[dto.GatewayMetadataWechatPayOptions] = rawOptions
	}
	p.UpdatedAt = time.Now().UTC()
}

// persistCreatedOrder stores the gateway's typed action and moves the payment to
// PENDING. A callback can settle the payment while the creation response is in
// flight; the conditional write and the re-read keep that settlement.
func (s *wechatPayService) persistCreatedOrder(ctx context.Context, p *payment.Payment, resp *interfaces.CheckoutProviderResponse) error {
	rawAction, err := json.Marshal(resp.NextAction)
	if err != nil {
		return ierr.WithError(err).
			WithHint("Failed to encode the gateway next action").
			WithReportableDetails(map[string]any{"payment_id": p.ID}).
			Mark(ierr.ErrSystem)
	}
	expiresAt := wechatPayExpiry(p)
	if resp.ExpiresAt != nil {
		expiresAt = resp.ExpiresAt.UTC()
	}

	for range 2 {
		statusBefore := p.PaymentStatus
		p.PaymentStatus = types.PaymentStatusPending
		if p.GatewayMetadata == nil {
			p.GatewayMetadata = types.Metadata{}
		}
		p.GatewayMetadata[dto.GatewayMetadataWechatPayAction] = string(rawAction)
		p.GatewayMetadata[dto.GatewayMetadataWechatPayExpiresAt] = expiresAt.Format(time.RFC3339)
		p.UpdatedAt = time.Now().UTC()

		err := s.PaymentRepo.UpdateWithExpectedStatus(ctx, p, statusBefore)
		if err == nil {
			recordPaymentTransition(ctx, p, statusBefore)
			s.Logger.Info(ctx, "wechatpay order created",
				"payment_id", p.ID,
				"invoice_id", p.DestinationID,
				"action_type", resp.NextAction.Type)
			return nil
		}
		if !ierr.IsVersionConflict(err) {
			return err
		}

		latest, getErr := s.PaymentRepo.Get(ctx, p.ID)
		if getErr != nil {
			return getErr
		}
		if latest.PaymentStatus == types.PaymentStatusSucceeded || latest.PaymentStatus.IsTerminal() {
			// A callback settled the payment first; its state wins and the action
			// has nobody left to serve.
			return nil
		}
		*p = *latest
	}

	return ierr.NewError("failed to record wechatpay order state").
		WithHint("The payment changed concurrently while the order was being created").
		WithReportableDetails(map[string]any{"payment_id": p.ID}).
		Mark(ierr.ErrVersionConflict)
}

// reconcilePayment asks the gateway what happened to the order bound to a local
// payment and applies the verified result. It is the recovery path for dropped
// callbacks and for creation responses that timed out: the order handle was
// persisted before the network call, so it is always available.
func (s *wechatPayService) reconcilePayment(ctx context.Context, p *payment.Payment) (*payment.Payment, error) {
	if p == nil {
		return nil, ierr.NewError("payment is required").Mark(ierr.ErrValidation)
	}
	orderID := lo.FromPtr(p.GatewayTrackingID)
	if orderID == "" {
		// No bound handle means no provider order to ask about.
		return p, nil
	}
	switch p.PaymentStatus {
	case types.PaymentStatusInitiated, types.PaymentStatusPending, types.PaymentStatusProcessing:
	default:
		return p, nil
	}

	client, err := s.IntegrationFactory.GetWechatPayClient(ctx)
	if err != nil {
		return p, err
	}

	tx, err := client.GetTransaction(ctx, orderID)
	if err != nil {
		if errors.Is(err, wechatpay.ErrOrderNotFound) {
			// The provider has no order under the persisted handle, so this intent
			// can never be paid. Only the fixed deadline turns that into a
			// failure; before it a not-yet-visible order is left alone.
			if time.Now().UTC().Before(wechatPayExpiry(p)) {
				return p, nil
			}
			return s.markExpired(ctx, p, "wechatpay order was not found at the provider")
		}
		if time.Now().UTC().Before(wechatPayExpiry(p)) {
			return p, err
		}
		// The query failed past the deadline (this includes an order the provider
		// reports as already closed). A close is still safe here because WeChat
		// Pay refuses to close a paid order, so a successful or already-closed
		// close confirms non-payment and turns an indeterminate order into a
		// settled outcome instead of leaving the payment pending forever.
		expired, expireErr := s.resolveUnpaidOrder(ctx, client, p, orderID)
		if expireErr != nil {
			return p, expireErr
		}
		switch expired.PaymentStatus {
		case types.PaymentStatusFailed, types.PaymentStatusSucceeded:
			return expired, nil
		}
		return p, err
	}

	// Identity first: no trade state — success, unpaid, or terminal — may drive
	// this payment before the response is bound to the local intent.
	if err := s.validateTransactionIdentity(ctx, p, tx); err != nil {
		return p, err
	}

	switch string(tx.TradeState) {
	case string(wechatpay.TradeStateSuccess):
		if err := s.validateSettledTransaction(p, tx); err != nil {
			return p, err
		}
		if err := s.applySettledTransaction(ctx, p, tx); err != nil {
			return p, err
		}
		return s.PaymentRepo.Get(ctx, p.ID)
	case string(wechatpay.TradeStateNotPay), string(wechatpay.TradeStateUserPaying):
		return s.resolveUnpaidOrder(ctx, client, p, orderID)
	case string(wechatpay.TradeStateClosed), string(wechatpay.TradeStateRevoked), string(wechatpay.TradeStatePayError):
		return s.markExpired(ctx, p, fmt.Sprintf("wechatpay order is %s", tx.TradeState))
	default:
		return p, nil
	}
}

// resolveUnpaidOrder handles an order the gateway still reports as unpaid. Past
// the fixed deadline it is closed — but only after the query that established it
// is still unpaid, and only a confirmed close is allowed to expire the local
// intent. An ambiguous close outcome leaves the payment untouched so a later
// reconciliation retries instead of guessing.
func (s *wechatPayService) resolveUnpaidOrder(ctx context.Context, client wechatpay.Gateway, p *payment.Payment, orderID string) (*payment.Payment, error) {
	if time.Now().UTC().Before(wechatPayExpiry(p)) {
		return p, nil
	}

	if err := client.CloseOrder(ctx, orderID); err != nil {
		// ORDERPAID means money arrived between the query and the close; any
		// other error leaves the outcome unknown. One re-query distinguishes the
		// two: a success settles, a definitive non-payable state expires,
		// anything else stays untouched.
		latest, queryErr := client.GetTransaction(ctx, orderID)
		if queryErr != nil {
			return p, nil
		}
		// The re-queried response must belong to this payment's order and the
		// tenant's connection before it may settle or expire anything.
		if err := s.validateTransactionIdentity(ctx, p, latest); err != nil {
			return p, err
		}
		switch string(latest.TradeState) {
		case string(wechatpay.TradeStateSuccess):
			if err := s.validateSettledTransaction(p, latest); err != nil {
				return p, err
			}
			if err := s.applySettledTransaction(ctx, p, latest); err != nil {
				return p, err
			}
			return s.PaymentRepo.Get(ctx, p.ID)
		case string(wechatpay.TradeStateClosed), string(wechatpay.TradeStateRevoked), string(wechatpay.TradeStatePayError):
			return s.markExpired(ctx, p, fmt.Sprintf("wechatpay order is %s", latest.TradeState))
		default:
			return p, nil
		}
	}

	return s.markExpired(ctx, p, "wechatpay order expired before payment")
}

// markExpired closes a local payment whose order is confirmed unpayable.
func (s *wechatPayService) markExpired(ctx context.Context, p *payment.Payment, reason string) (*payment.Payment, error) {
	updated, err := NewPaymentService(s.ServiceParams).UpdatePayment(ctx, p.ID, dto.UpdatePaymentRequest{
		PaymentStatus: lo.ToPtr(string(types.PaymentStatusFailed)),
		FailedAt:      lo.ToPtr(time.Now().UTC()),
		ErrorMessage:  lo.ToPtr(reason),
	})
	if err != nil {
		if ierr.IsVersionConflict(err) {
			if current, getErr := s.PaymentRepo.Get(ctx, p.ID); getErr == nil {
				return current, nil
			}
		}
		return p, err
	}
	return updated.ToPayment(), nil
}

// settleTransaction records a verified successful transaction, shared by the
// signed callback and the status query. It reuses PaymentLifecycle, so the
// payment transition, its events, and the invoice reconciliation are exactly the
// ones every other gateway produces.
func (s *wechatPayService) settleTransaction(ctx context.Context, p *payment.Payment, tx *wechatpay.Transaction) error {
	if p == nil || tx == nil {
		return ierr.NewError("payment and transaction are required").
			WithHint("A settlement needs both the local payment and the gateway transaction").
			Mark(ierr.ErrValidation)
	}
	if err := s.validateTransactionIdentity(ctx, p, tx); err != nil {
		return err
	}
	if err := s.validateSettledTransaction(p, tx); err != nil {
		return err
	}
	return s.applySettledTransaction(ctx, p, tx)
}

// applySettledTransaction records an already-verified successful transaction
// through PaymentLifecycle.
func (s *wechatPayService) applySettledTransaction(ctx context.Context, p *payment.Payment, tx *wechatpay.Transaction) error {
	// Settlement runs through the public service APIs, which read the payment
	// back; mark the context so those reads stay passive instead of re-querying
	// the gateway mid-settlement.
	ctx = withWechatPaySettling(ctx)

	// One transaction spans the payment transition, the attempt, and the invoice
	// reconciliation. A concurrent delivery either loses the conditional payment
	// write and re-reads the settled state, or waits for this commit — it can
	// never observe a payment that is paid but unreconciled. When already inside
	// a transaction, WithTx reuses it.
	return s.DB.WithTx(ctx, func(txCtx context.Context) error {
		// The lifecycle below reads and rewrites the invoice payment state, so
		// the invoice row is locked before anything else in this transaction:
		// two deliveries for the same invoice cannot interleave that
		// read-modify-write. The repair path reuses this lock — its nested
		// WithTx reuses the transaction and re-locks the same row.
		if p.DestinationType == types.PaymentDestinationTypeInvoice {
			if _, err := s.InvoiceRepo.GetForUpdate(txCtx, p.DestinationID); err != nil {
				return err
			}
		}

		latest, err := s.PaymentRepo.Get(txCtx, p.ID)
		if err != nil {
			return err
		}

		switch latest.PaymentStatus {
		case types.PaymentStatusSucceeded:
			// Already settled by an earlier delivery or a query; make sure the
			// invoice side is reconciled too, then report success again.
			return s.ensureInvoiceReconciled(txCtx, latest)
		case types.PaymentStatusRefunded, types.PaymentStatusVoided, types.PaymentStatusOverpaid, types.PaymentStatusPartiallyRefunded:
			// Money was taken and, where applicable, has already been given back;
			// a redelivered success callback adds nothing.
			return nil
		case types.PaymentStatusFailed:
			return ierr.NewError("wechatpay reports a paid order for a failed local payment").
				WithHint("The order was settled at the provider after the local payment was failed; manual reconciliation is required").
				WithReportableDetails(map[string]any{
					"payment_id":             latest.ID,
					"gateway_transaction_id": tx.TransactionID,
					"gateway_tracking_id":    tx.OutTradeNo,
				}).
				Mark(ierr.ErrInvalidOperation)
		}

		lifecycle := payments.NewPaymentLifecycle(NewPaymentService(s.ServiceParams), NewInvoiceService(s.ServiceParams), s.Logger)
		err = lifecycle.RecordPaymentSuccess(txCtx, payments.RecordPaymentSuccessParams{
			FlexpricePaymentID: latest.ID,
			GatewayPaymentID:   tx.TransactionID,
			SucceededAt:        wechatPaySuccessTime(tx.SuccessTime),
		})
		if err != nil {
			// A concurrent delivery in another transaction may have settled the
			// payment first; converge on the recorded state instead of failing a
			// payment that is already paid.
			if current, getErr := s.PaymentRepo.Get(txCtx, latest.ID); getErr == nil && current.PaymentStatus == types.PaymentStatusSucceeded {
				return s.ensureInvoiceReconciled(txCtx, current)
			}
			return err
		}

		if err := lifecycle.RecordSucceededAttempt(txCtx, payments.RecordSucceededAttemptParams{
			FlexpricePaymentID: latest.ID,
			GatewayPaymentID:   tx.TransactionID,
		}); err != nil {
			return err
		}

		return s.ensureInvoiceReconciled(txCtx, latest)
	})
}

// validateTransactionIdentity binds a verified gateway transaction to the local
// intent before any trade-state branch runs: the merchant order number must be
// the one stored on the payment, and the merchant and app must be the tenant's
// scoped connection. Every state is checked, so a terminal or unpaid response
// can never drive a payment bound to another order or merchant.
func (s *wechatPayService) validateTransactionIdentity(ctx context.Context, p *payment.Payment, tx *wechatpay.Transaction) error {
	if tx == nil {
		return ierr.NewError("wechatpay transaction is missing").
			WithHint("The provider returned no transaction to verify").
			WithReportableDetails(map[string]any{"payment_id": p.ID}).
			Mark(ierr.ErrValidation)
	}
	if lo.FromPtr(p.GatewayTrackingID) != tx.OutTradeNo {
		return ierr.NewError("wechatpay transaction does not match the bound order").
			WithHint("The transaction's merchant order number differs from the one bound to this payment").
			WithReportableDetails(map[string]any{
				"payment_id":           p.ID,
				"bound_order_id":       lo.FromPtr(p.GatewayTrackingID),
				"transaction_order_id": tx.OutTradeNo,
			}).
			Mark(ierr.ErrValidation)
	}

	conn, err := s.ConnectionRepo.GetByProvider(ctx, types.SecretProviderWechatPay)
	if err != nil {
		return ierr.WithError(err).
			WithHint("WeChat Pay connection is not configured for this environment").
			Error()
	}
	if conn == nil || conn.EncryptedSecretData.WechatPay == nil {
		return ierr.NewError("wechatpay connection metadata is missing").
			WithHint("Configure the WeChat Pay merchant connection before accepting payments").
			Mark(ierr.ErrNotFound)
	}
	if conn.EncryptedSecretData.WechatPay.AppID != tx.AppID {
		return ierr.NewError("wechatpay transaction appid does not match the connection").
			WithHint("The transaction was issued for a different WeChat app").
			WithReportableDetails(map[string]any{"payment_id": p.ID}).
			Mark(ierr.ErrValidation)
	}
	if conn.EncryptedSecretData.WechatPay.MerchantID != tx.MerchantID {
		return ierr.NewError("wechatpay transaction merchant does not match the connection").
			WithHint("The transaction was issued for a different WeChat merchant").
			WithReportableDetails(map[string]any{"payment_id": p.ID}).
			Mark(ierr.ErrValidation)
	}
	return nil
}

// validateSettledTransaction checks the fields only a successful transaction
// carries. WeChat's schema omits the amount block for unpaid or closed orders,
// so the transaction id and amount checks apply to SUCCESS alone.
func (s *wechatPayService) validateSettledTransaction(p *payment.Payment, tx *wechatpay.Transaction) error {
	if tx.TransactionID == "" {
		return ierr.NewError("wechatpay transaction id is missing").
			WithHint("A settled transaction must carry the provider's transaction id").
			WithReportableDetails(map[string]any{"payment_id": p.ID}).
			Mark(ierr.ErrValidation)
	}
	if !strings.EqualFold(tx.Amount.Currency, "CNY") {
		return ierr.NewError("wechatpay transaction is not in CNY").
			WithHint("Only CNY payments are supported").
			WithReportableDetails(map[string]any{
				"payment_id":       p.ID,
				"tx_currency":      tx.Amount.Currency,
				"payment_currency": p.Currency,
			}).
			Mark(ierr.ErrValidation)
	}
	wantCents, err := wechatPayCents(p.Amount)
	if err != nil {
		return err
	}
	if tx.Amount.Total != wantCents {
		return ierr.NewError("wechatpay transaction amount does not match the payment").
			WithHint("The amount settled at the provider differs from the local payment amount").
			WithReportableDetails(map[string]any{
				"payment_id":        p.ID,
				"tx_total_fen":      tx.Amount.Total,
				"payment_total_fen": wantCents,
			}).
			Mark(ierr.ErrValidation)
	}
	return nil
}

// ensureInvoiceReconciled makes the invoice side converge for a payment that is
// already durably SUCCEEDED. The lifecycle settles payment and invoice together
// on the happy path; this covers a delivery that stopped between the two writes,
// so a paid payment can never leave its invoice permanently unreconciled. The
// invoice row is locked and re-checked first so two concurrent repairs cannot
// credit the same payment twice.
func (s *wechatPayService) ensureInvoiceReconciled(ctx context.Context, p *payment.Payment) error {
	if p.DestinationType != types.PaymentDestinationTypeInvoice {
		return nil
	}
	// Reuses the caller's transaction when one is open (settlement always has
	// one); the lock only holds for the duration of that transaction.
	return s.DB.WithTx(ctx, func(txCtx context.Context) error {
		inv, err := s.InvoiceRepo.GetForUpdate(txCtx, p.DestinationID)
		if err != nil {
			return err
		}
		switch inv.PaymentStatus {
		case types.PaymentStatusSucceeded, types.PaymentStatusOverpaid:
			return nil
		}
		if err := NewInvoiceService(s.ServiceParams).ReconcilePaymentStatus(txCtx, p.DestinationID, types.PaymentStatusSucceeded, &p.Amount); err != nil {
			return ierr.WithError(err).
				WithHint("Payment succeeded but invoice reconciliation failed").
				WithReportableDetails(map[string]any{
					"payment_id": p.ID,
					"invoice_id": p.DestinationID,
				}).
				Mark(ierr.ErrSystem)
		}
		s.Logger.Info(txCtx, "reconciled invoice for already-succeeded wechatpay payment",
			"payment_id", p.ID,
			"invoice_id", p.DestinationID)
		return nil
	})
}

// QueryPayment reconciles one local payment against WeChat Pay.
func (s *wechatPayService) QueryPayment(ctx context.Context, paymentID string) (*dto.PaymentResponse, error) {
	if paymentID == "" {
		return nil, ierr.NewError("payment_id is required").
			WithHint("Payment ID is required").
			Mark(ierr.ErrValidation)
	}
	p, err := s.PaymentRepo.Get(ctx, paymentID)
	if err != nil {
		return nil, err
	}
	updated, err := s.reconcilePayment(ctx, p)
	if err != nil {
		return nil, err
	}
	return dto.NewPaymentResponse(updated), nil
}

// createWechatPayIntent admits one WeChat Pay intent for an existing invoice.
// The invoice row is locked for the authoritative validation, the live-intent
// check, and the payment insert, so concurrent requests — even on different
// channels or with different keys — cannot both observe "no live intent" and
// mint two payable orders. The gateway is contacted only after this commits,
// by the caller's ProcessPayment step.
func (s *paymentService) createWechatPayIntent(ctx context.Context, req *dto.CreatePaymentRequest, p *payment.Payment) (*dto.PaymentResponse, error) {
	if p.PaymentMethodType != types.PaymentMethodTypePaymentLink {
		return nil, ierr.NewError("wechatpay supports payment-link payments only").
			WithHint("WeChat Pay has no saved-card, wallet, or card-charge flow; use payment_method_type PAYMENT_LINK").
			WithReportableDetails(map[string]interface{}{
				"payment_method_type": p.PaymentMethodType,
			}).
			Mark(ierr.ErrValidation)
	}
	if p.DestinationType != types.PaymentDestinationTypeInvoice {
		return nil, ierr.NewError("wechatpay payments must target an invoice").
			WithHint("WeChat Pay supports one-off invoice payments only").
			WithReportableDetails(map[string]interface{}{
				"destination_type": p.DestinationType,
			}).
			Mark(ierr.ErrValidation)
	}

	if p.IdempotencyKey == "" {
		p.IdempotencyKey = s.idempGen.GenerateKey(idempotency.ScopePayment, paymentIdempotencyKeyInputs(p, req))
	}

	var live *payment.Payment
	err := s.DB.WithTx(ctx, func(txCtx context.Context) error {
		// Locking the invoice is what serializes admission: the second of two
		// concurrent requests reads the live-intent check only after the first
		// has committed its row.
		inv, err := s.InvoiceRepo.GetForUpdate(txCtx, p.DestinationID)
		if err != nil {
			return err
		}
		if err := s.validateInvoicePaymentEligibility(txCtx, inv, req); err != nil {
			return err
		}
		if err := validateWechatPayInvoicePayment(inv, p.Amount, p.Currency); err != nil {
			return err
		}

		existing, err := s.findLiveWechatPayIntent(txCtx, p)
		if err != nil {
			return err
		}
		if existing != nil {
			live = existing
			return nil
		}

		if err := p.Validate(); err != nil {
			return err
		}
		return s.PaymentRepo.Create(txCtx, p)
	})
	if err != nil {
		if payment.IsIdempotencyKeyConflict(err) {
			// An identical intent committed concurrently; return its row rather
			// than the conflict.
			existing, fetchErr := s.PaymentRepo.GetByIdempotencyKey(ctx, p.IdempotencyKey)
			if fetchErr != nil {
				return nil, err
			}
			return dto.NewPaymentResponse(existing), nil
		}
		return nil, err
	}

	if live != nil {
		s.Logger.Info(ctx, "returning existing live wechatpay intent",
			"payment_id", live.ID,
			"invoice_id", p.DestinationID)
		return s.resolveLiveWechatPayIntent(ctx, req, live)
	}

	recordPaymentTransition(ctx, p, "")
	s.publishSystemEvent(ctx, types.WebhookEventPaymentCreated, p.ID)

	// The network call happens after the admission transaction has committed.
	if req.ProcessPayment {
		processed, err := NewPaymentProcessorService(s.ServiceParams).ProcessPayment(ctx, p.ID)
		if err != nil {
			return nil, ierr.WithError(err).
				WithHint("Failed to process payment").
				WithReportableDetails(map[string]interface{}{
					"payment_id": p.ID,
				}).
				Mark(ierr.ErrInvalidOperation)
		}
		return dto.NewPaymentResponse(processed), nil
	}

	return dto.NewPaymentResponse(p), nil
}

// resolveLiveWechatPayIntent answers a repeated request with the existing
// payable intent. The admission snapshot can be stale — the winning request may
// have moved the row into PROCESSING or settled it between the check and this
// call — so the current row decides: only a row still INITIATED is healed, and
// losing the processing race is reported as the record itself, never as an
// error. A row already in flight is a valid answer: the caller can read its
// action/status while the order is being created.
func (s *paymentService) resolveLiveWechatPayIntent(ctx context.Context, req *dto.CreatePaymentRequest, live *payment.Payment) (*dto.PaymentResponse, error) {
	current, err := s.PaymentRepo.Get(ctx, live.ID)
	if err != nil {
		return nil, err
	}

	if req.ProcessPayment && current.PaymentStatus == types.PaymentStatusInitiated {
		processed, err := NewPaymentProcessorService(s.ServiceParams).ProcessPayment(ctx, current.ID)
		if err != nil {
			// The row may have moved on between this read and the processor's
			// status gate: another request owns the processing. When the current
			// row proves that, it is the answer, not an error.
			latest, getErr := s.PaymentRepo.Get(ctx, current.ID)
			if getErr == nil {
				switch latest.PaymentStatus {
				case types.PaymentStatusProcessing, types.PaymentStatusPending,
					types.PaymentStatusSucceeded, types.PaymentStatusOverpaid:
					return dto.NewPaymentResponse(latest), nil
				}
			}
			return nil, ierr.WithError(err).
				WithHint("Failed to process the existing wechatpay payment").
				WithReportableDetails(map[string]interface{}{
					"payment_id": current.ID,
				}).
				Mark(ierr.ErrInvalidOperation)
		}
		return dto.NewPaymentResponse(processed), nil
	}

	return dto.NewPaymentResponse(current), nil
}

// findLiveWechatPayIntent returns an existing payable WeChat Pay payment for the
// same invoice and amount, so repeated create requests cannot mint a second
// order that could be paid alongside the first. Expired intents are ignored:
// the query path closes them at the provider and they are never reused.
func (s *paymentService) findLiveWechatPayIntent(ctx context.Context, p *payment.Payment) (*payment.Payment, error) {
	filter := types.NewNoLimitPaymentFilter()
	filter.DestinationType = lo.ToPtr(string(types.PaymentDestinationTypeInvoice))
	filter.DestinationID = lo.ToPtr(p.DestinationID)
	filter.PaymentGateway = lo.ToPtr(string(types.PaymentGatewayTypeWechatPay))

	candidates, err := s.PaymentRepo.List(ctx, filter)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	var live *payment.Payment
	for _, candidate := range candidates {
		if candidate.PaymentMethodType != types.PaymentMethodTypePaymentLink {
			continue
		}
		switch candidate.PaymentStatus {
		case types.PaymentStatusInitiated, types.PaymentStatusPending, types.PaymentStatusProcessing:
		default:
			continue
		}
		if !candidate.Amount.Equal(p.Amount) || !types.IsMatchingCurrency(candidate.Currency, p.Currency) {
			continue
		}
		if !now.Before(wechatPayExpiry(candidate)) {
			continue
		}
		if live == nil || candidate.CreatedAt.After(live.CreatedAt) {
			live = candidate
		}
	}
	if live == nil {
		return nil, nil
	}

	// A live order on a different channel cannot serve this request, and letting
	// a second order exist for the same invoice risks collecting twice.
	requestedOptions, _, err := wechatPayOptionsFor(p)
	if err != nil {
		return nil, err
	}
	liveOptions, _, err := wechatPayOptionsFor(live)
	if err != nil {
		return nil, err
	}
	if requestedOptions.EffectivePaymentType() != liveOptions.EffectivePaymentType() {
		return nil, ierr.NewError("an open wechatpay payment already exists on another channel").
			WithHintf("Invoice %s already has an open WeChat Pay payment using the %s channel; complete it, let it expire, or reuse that payment.", p.DestinationID, liveOptions.EffectivePaymentType()).
			WithReportableDetails(map[string]any{
				"invoice_id":          p.DestinationID,
				"existing_payment_id": live.ID,
				"existing_channel":    liveOptions.EffectivePaymentType(),
				"requested_channel":   requestedOptions.EffectivePaymentType(),
			}).
			Mark(ierr.ErrValidation)
	}
	return live, nil
}

// paymentByOutTradeNo resolves the local payment bound to a provider order. The
// lookup is tenant/environment scoped through the context, which is what binds
// an incoming callback to the right tenant.
func (s *wechatPayService) paymentByOutTradeNo(ctx context.Context, outTradeNo string) (*payment.Payment, error) {
	if outTradeNo == "" {
		return nil, ierr.NewError("wechatpay order id is missing").
			WithHint("The notification carried no merchant order number").
			Mark(ierr.ErrValidation)
	}
	filter := &types.PaymentFilter{
		QueryFilter:       &types.QueryFilter{Limit: lo.ToPtr(1)},
		GatewayTrackingID: lo.ToPtr(outTradeNo),
		PaymentGateway:    lo.ToPtr(string(types.PaymentGatewayTypeWechatPay)),
	}
	paymentsList, err := s.PaymentRepo.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	if len(paymentsList) == 0 {
		return nil, ierr.NewError("no payment is bound to this wechatpay order").
			WithHint("The notification references an order this environment never created").
			WithReportableDetails(map[string]any{"out_trade_no": outTradeNo}).
			Mark(ierr.ErrNotFound)
	}
	return paymentsList[0], nil
}

// wechatPayOptionsFor loads the channel options persisted with the payment,
// defaulting to native and validating before any network call.
func wechatPayOptionsFor(p *payment.Payment) (*types.WechatPayOptions, string, error) {
	raw := ""
	if p.GatewayMetadata != nil {
		raw = p.GatewayMetadata[dto.GatewayMetadataWechatPayOptions]
	}
	options := &types.WechatPayOptions{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), options); err != nil {
			return nil, "", ierr.WithError(err).
				WithHint("The stored WeChat Pay options are malformed").
				WithReportableDetails(map[string]any{"payment_id": p.ID}).
				Mark(ierr.ErrValidation)
		}
	}
	if err := options.Validate(); err != nil {
		return nil, "", err
	}
	if raw == "" {
		marshaled, err := json.Marshal(options)
		if err != nil {
			return nil, "", ierr.WithError(err).Mark(ierr.ErrSystem)
		}
		raw = string(marshaled)
	}
	return options, raw, nil
}

// wechatPayExpiry returns the fixed deadline of a payment's order: persisted at
// the first attempt, derived from the row's creation time for older rows.
func wechatPayExpiry(p *payment.Payment) time.Time {
	if p.GatewayMetadata != nil {
		if raw := p.GatewayMetadata[dto.GatewayMetadataWechatPayExpiresAt]; raw != "" {
			if ts, err := time.Parse(time.RFC3339, raw); err == nil {
				return ts.UTC()
			}
		}
	}
	return p.CreatedAt.UTC().Add(types.CheckoutPaymentProviderWechatPay.LinkExpiry())
}

// wechatPaySuccessTime parses the gateway's RFC3339 success time, falling back
// to now when the provider omitted or malformed it.
func wechatPaySuccessTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

// validateWechatPayInvoicePayment enforces the WeChat Pay invariants against an
// invoice: CNY only, a positive whole number of fen that fits an int64, and
// exactly the outstanding amount, so an order can never over- or under-collect.
func validateWechatPayInvoicePayment(inv *invoice.Invoice, amount decimal.Decimal, currency string) error {
	if inv == nil {
		return ierr.NewError("invoice is required").
			WithHint("A WeChat Pay payment must target an invoice").
			Mark(ierr.ErrValidation)
	}
	if inv.InvoiceStatus == types.InvoiceStatusVoided {
		return ierr.NewError("invoice is voided").
			WithHint("Cannot pay a voided invoice").
			WithReportableDetails(map[string]any{"invoice_id": inv.ID}).
			Mark(ierr.ErrValidation)
	}
	if inv.PaymentStatus == types.PaymentStatusSucceeded || inv.PaymentStatus == types.PaymentStatusOverpaid {
		return ierr.NewError("invoice is already paid").
			WithHint("Cannot create a WeChat Pay order for an already paid invoice").
			WithReportableDetails(map[string]any{"invoice_id": inv.ID}).
			Mark(ierr.ErrValidation)
	}
	if !strings.EqualFold(currency, "CNY") {
		return ierr.NewError("wechatpay only supports CNY").
			WithHint("Use CNY for WeChat Pay payments").
			WithReportableDetails(map[string]any{"currency": currency}).
			Mark(ierr.ErrValidation)
	}
	if !types.IsMatchingCurrency(inv.Currency, currency) {
		return ierr.NewError("invoice currency does not match the wechatpay payment currency").
			WithHint("WeChat Pay payments and their invoice must both be in CNY").
			WithReportableDetails(map[string]any{
				"invoice_currency": inv.Currency,
				"payment_currency": currency,
			}).
			Mark(ierr.ErrValidation)
	}
	if _, err := wechatPayCents(amount); err != nil {
		return err
	}

	outstanding := inv.AmountDue.Sub(inv.AmountPaid)
	if !outstanding.IsPositive() {
		return ierr.NewError("invoice has no outstanding amount").
			WithHint("There is nothing left to pay on this invoice").
			WithReportableDetails(map[string]any{
				"invoice_id":  inv.ID,
				"amount_due":  inv.AmountDue,
				"amount_paid": inv.AmountPaid,
			}).
			Mark(ierr.ErrValidation)
	}
	if !amount.Equal(outstanding) {
		return ierr.NewError("wechatpay payments must cover the full outstanding amount").
			WithHintf("The invoice outstanding amount is %s %s; a WeChat Pay payment must equal it exactly.", outstanding, inv.Currency).
			WithReportableDetails(map[string]any{
				"invoice_id":          inv.ID,
				"invoice_outstanding": outstanding,
				"payment_amount":      amount,
			}).
			Mark(ierr.ErrValidation)
	}
	return nil
}

// wechatPayCents converts a major-unit amount into the integer fen WeChat Pay
// uses, rejecting fractional fen, non-positive amounts, and overflow.
func wechatPayCents(amount decimal.Decimal) (int64, error) {
	fen := amount.Mul(decimal.NewFromInt(100))
	if !fen.IsInteger() {
		return 0, ierr.NewError("wechatpay amount must be a whole number of fen").
			WithHint("WeChat Pay accepts amounts with two decimal places at most").
			WithReportableDetails(map[string]any{"amount": amount.String()}).
			Mark(ierr.ErrValidation)
	}
	if fen.LessThanOrEqual(decimal.Zero) {
		return 0, ierr.NewError("wechatpay amount must be positive").
			WithHint("WeChat Pay amounts must be greater than zero").
			WithReportableDetails(map[string]any{"amount": amount.String()}).
			Mark(ierr.ErrValidation)
	}
	if fen.GreaterThan(decimal.NewFromInt(math.MaxInt64)) {
		return 0, ierr.NewError("wechatpay amount is too large").
			WithHint("WeChat Pay amounts must fit a signed 64-bit fen value").
			WithReportableDetails(map[string]any{"amount": amount.String()}).
			Mark(ierr.ErrValidation)
	}
	return fen.IntPart(), nil
}
