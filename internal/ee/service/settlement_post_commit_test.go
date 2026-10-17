package service

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/flexprice/flexprice/ent"
	"github.com/flexprice/flexprice/internal/api/dto"
	"github.com/flexprice/flexprice/internal/domain/connection"
	"github.com/flexprice/flexprice/internal/domain/payment"
	"github.com/flexprice/flexprice/internal/logger"
	"github.com/flexprice/flexprice/internal/testutil"
	"github.com/flexprice/flexprice/internal/types"
	webhookDto "github.com/flexprice/flexprice/internal/webhook/dto"
	webhookPublisher "github.com/flexprice/flexprice/internal/webhook/publisher"
	"github.com/samber/lo"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postCommitRecordingPublisher records every publish together with the context
// it arrived on, so tests can assert both the event and whether the work still
// carried the caller's transaction.
type postCommitRecordingPublisher struct {
	mu     sync.Mutex
	ctxs   []context.Context
	events []*types.WebhookEvent
}

var _ webhookPublisher.WebhookPublisher = (*postCommitRecordingPublisher)(nil)

func (p *postCommitRecordingPublisher) PublishWebhook(ctx context.Context, event *types.WebhookEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ctxs = append(p.ctxs, ctx)
	p.events = append(p.events, event)
	return nil
}

func (p *postCommitRecordingPublisher) Close() error { return nil }

func (p *postCommitRecordingPublisher) published() []*types.WebhookEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*types.WebhookEvent(nil), p.events...)
}

func (p *postCommitRecordingPublisher) publishContexts() []context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]context.Context(nil), p.ctxs...)
}

// dispatchRecordingConnectionRepo records each mark-paid dispatch; a nil
// connection list keeps the dispatch off Temporal.
type dispatchRecordingConnectionRepo struct {
	connection.Repository
	mu    sync.Mutex
	calls []context.Context
}

var _ connection.Repository = (*dispatchRecordingConnectionRepo)(nil)

func (r *dispatchRecordingConnectionRepo) ListAllPublished(ctx context.Context) ([]*connection.Connection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, ctx)
	return nil, nil
}

func (r *dispatchRecordingConnectionRepo) callContexts() []context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]context.Context(nil), r.calls...)
}

const (
	postCommitTestTenant = "tenant_post_commit_test"
	postCommitTestEnv    = "env_post_commit_test"
)

func postCommitTestContext() context.Context {
	ctx := types.SetTenantID(context.Background(), postCommitTestTenant)
	return types.SetEnvironmentID(ctx, postCommitTestEnv)
}

// postCommitTxContext simulates a write transaction at the test seam: the
// transaction marker plus the post-commit hook list DB.WithTx installs.
func postCommitTxContext(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, types.CtxDBTransaction, &ent.Tx{})
	return types.WithPostCommitHooks(ctx)
}

// newPostCommitPaymentService wires the real payment service against an
// in-memory payment store and a recording publisher.
func newPostCommitPaymentService(publisher webhookPublisher.WebhookPublisher) (PaymentService, payment.Repository) {
	repo := testutil.NewInMemoryPaymentStore()
	svc := NewPaymentService(ServiceParams{
		Logger:           logger.NewNoopLogger(),
		PaymentRepo:      repo,
		WebhookPublisher: publisher,
	})
	return svc, repo
}

func seedPostCommitPayment(t *testing.T, ctx context.Context, repo payment.Repository, id string) {
	t.Helper()
	require.NoError(t, repo.Create(ctx, &payment.Payment{
		ID:                id,
		DestinationType:   types.PaymentDestinationTypeInvoice,
		DestinationID:     "inv_post_commit",
		PaymentMethodType: types.PaymentMethodTypePaymentLink,
		PaymentGateway:    lo.ToPtr(string(types.PaymentGatewayTypeWechatPay)),
		PaymentStatus:     types.PaymentStatusInitiated,
		Amount:            decimal.NewFromInt(100),
		Currency:          "CNY",
		EnvironmentID:     types.GetEnvironmentID(ctx),
		BaseModel:         types.GetDefaultBaseModel(ctx),
	}))
}

// The WeChat settlement path calls UpdatePayment inside DB.WithTx. The event
// must not reach the producer before the commit lands — a consumer reading the
// payment at delivery time would serve the pre-settlement state — and once the
// commit lands the transition publishes exactly once.
func TestUpdatePaymentQueuesWebhookUntilCommit(t *testing.T) {
	ctx := postCommitTestContext()
	publisher := &postCommitRecordingPublisher{}
	svc, repo := newPostCommitPaymentService(publisher)
	seedPostCommitPayment(t, ctx, repo, "pay_post_commit_settle")

	txCtx := postCommitTxContext(ctx)

	resp, err := svc.UpdatePayment(txCtx, "pay_post_commit_settle", dto.UpdatePaymentRequest{
		PaymentStatus: lo.ToPtr(string(types.PaymentStatusSucceeded)),
	})
	require.NoError(t, err)
	require.Equal(t, types.PaymentStatusSucceeded, resp.PaymentStatus)
	assert.Empty(t, publisher.published(), "no webhook may be published before the commit")

	types.RunPostCommitHooks(txCtx)

	events := publisher.published()
	require.Len(t, events, 1, "the committed transition publishes once")
	assert.Equal(t, types.WebhookEventPaymentUpdated, events[0].EventName)
	assert.Equal(t, types.SystemEntityTypePayment, events[0].EntityType)
	assert.Equal(t, "pay_post_commit_settle", events[0].EntityID)
	assert.Equal(t, postCommitTestTenant, events[0].TenantID)
	assert.Equal(t, postCommitTestEnv, events[0].EnvironmentID)

	var payload webhookDto.InternalPaymentEvent
	require.NoError(t, json.Unmarshal(events[0].Payload, &payload))
	assert.Equal(t, "pay_post_commit_settle", payload.PaymentID)
	assert.Equal(t, postCommitTestTenant, payload.TenantID)

	contexts := publisher.publishContexts()
	require.Len(t, contexts, 1)
	assert.Nil(t, contexts[0].Value(types.CtxDBTransaction),
		"the queued publish must run without the committed transaction")

	// The commit drains the hooks; a later drain must not republish.
	types.RunPostCommitHooks(txCtx)
	assert.Len(t, publisher.published(), 1)
}

// A settlement transaction that rolls back must emit nothing: the event would
// announce a state that never committed.
func TestUpdatePaymentRolledBackTransitionPublishesNothing(t *testing.T) {
	ctx := postCommitTestContext()
	publisher := &postCommitRecordingPublisher{}
	svc, repo := newPostCommitPaymentService(publisher)
	seedPostCommitPayment(t, ctx, repo, "pay_post_commit_rollback")

	txCtx := postCommitTxContext(ctx)

	_, err := svc.UpdatePayment(txCtx, "pay_post_commit_rollback", dto.UpdatePaymentRequest{
		PaymentStatus: lo.ToPtr(string(types.PaymentStatusSucceeded)),
	})
	require.NoError(t, err)

	types.DiscardPostCommitHooks(txCtx)

	assert.Empty(t, publisher.published(), "a rolled-back transition must not emit a webhook")
	assert.Empty(t, publisher.publishContexts())
}

// Outside a transaction the existing immediate behavior is kept: a caller that
// never opened one must not wait for a commit that will never come.
func TestUpdatePaymentOutsideTransactionPublishesImmediately(t *testing.T) {
	ctx := postCommitTestContext()
	publisher := &postCommitRecordingPublisher{}
	svc, repo := newPostCommitPaymentService(publisher)
	seedPostCommitPayment(t, ctx, repo, "pay_post_commit_direct")

	_, err := svc.UpdatePayment(ctx, "pay_post_commit_direct", dto.UpdatePaymentRequest{
		PaymentStatus: lo.ToPtr(string(types.PaymentStatusSucceeded)),
	})
	require.NoError(t, err)

	events := publisher.published()
	require.Len(t, events, 1, "no transaction means no commit to wait for")
	assert.Equal(t, types.WebhookEventPaymentUpdated, events[0].EventName)

	contexts := publisher.publishContexts()
	require.Len(t, contexts, 1)
	assert.Equal(t, postCommitTestTenant, types.GetTenantID(contexts[0]))
	assert.Nil(t, contexts[0].Value(types.CtxDBTransaction))
}

// The settlement transaction reconciles the invoice through
// ReconcilePaymentStatus, which publishes invoice.update.payment on the
// transaction context.
func TestInvoicePublishSystemEventQueuedUntilCommit(t *testing.T) {
	ctx := postCommitTestContext()
	publisher := &postCommitRecordingPublisher{}
	invSvc := NewInvoiceService(ServiceParams{
		Logger:           logger.NewNoopLogger(),
		WebhookPublisher: publisher,
	}).(*invoiceService)

	txCtx := postCommitTxContext(ctx)

	invSvc.publishSystemEvent(txCtx, types.WebhookEventInvoiceUpdatePayment, "inv_post_commit")

	assert.Empty(t, publisher.published(), "no invoice webhook may be published before the commit")

	types.RunPostCommitHooks(txCtx)

	events := publisher.published()
	require.Len(t, events, 1, "the committed transition publishes once")
	assert.Equal(t, types.WebhookEventInvoiceUpdatePayment, events[0].EventName)
	assert.Equal(t, types.SystemEntityTypeInvoice, events[0].EntityType)
	assert.Equal(t, "inv_post_commit", events[0].EntityID)
	assert.Equal(t, postCommitTestTenant, events[0].TenantID)
	assert.Equal(t, postCommitTestEnv, events[0].EnvironmentID)

	var payload struct {
		InvoiceID string `json:"invoice_id"`
		TenantID  string `json:"tenant_id"`
	}
	require.NoError(t, json.Unmarshal(events[0].Payload, &payload))
	assert.Equal(t, "inv_post_commit", payload.InvoiceID)
	assert.Equal(t, postCommitTestTenant, payload.TenantID)

	contexts := publisher.publishContexts()
	require.Len(t, contexts, 1)
	assert.Nil(t, contexts[0].Value(types.CtxDBTransaction),
		"the queued publish must run without the committed transaction")
}

func TestInvoicePublishSystemEventDroppedOnRollback(t *testing.T) {
	ctx := postCommitTestContext()
	publisher := &postCommitRecordingPublisher{}
	invSvc := NewInvoiceService(ServiceParams{
		Logger:           logger.NewNoopLogger(),
		WebhookPublisher: publisher,
	}).(*invoiceService)

	txCtx := postCommitTxContext(ctx)

	invSvc.publishSystemEvent(txCtx, types.WebhookEventInvoiceUpdatePayment, "inv_post_commit_rollback")

	types.DiscardPostCommitHooks(txCtx)

	assert.Empty(t, publisher.published(), "a rolled-back transition must not emit a webhook")
	assert.Empty(t, publisher.publishContexts())
}

// The WeChat refund path settles or fails through the refund service, whose
// callback and query paths can run inside a transaction.
func TestRefundPublishSystemEventQueuedUntilCommit(t *testing.T) {
	ctx := postCommitTestContext()
	publisher := &postCommitRecordingPublisher{}
	refundSvc := NewRefundService(ServiceParams{
		Logger:           logger.NewNoopLogger(),
		WebhookPublisher: publisher,
	}).(*refundService)

	txCtx := postCommitTxContext(ctx)

	refundSvc.publishSystemEvent(txCtx, types.WebhookEventRefundSucceeded, "ref_post_commit")

	assert.Empty(t, publisher.published(), "no refund webhook may be published before the commit")

	types.RunPostCommitHooks(txCtx)

	events := publisher.published()
	require.Len(t, events, 1, "the committed transition publishes once")
	assert.Equal(t, types.WebhookEventRefundSucceeded, events[0].EventName)
	assert.Equal(t, types.SystemEntityTypeRefund, events[0].EntityType)
	assert.Equal(t, "ref_post_commit", events[0].EntityID)

	var payload webhookDto.InternalRefundEvent
	require.NoError(t, json.Unmarshal(events[0].Payload, &payload))
	assert.Equal(t, "ref_post_commit", payload.RefundID)
	assert.Equal(t, postCommitTestTenant, payload.TenantID)

	contexts := publisher.publishContexts()
	require.Len(t, contexts, 1)
	assert.Nil(t, contexts[0].Value(types.CtxDBTransaction),
		"the queued publish must run without the committed transaction")
}

func TestRefundPublishSystemEventDroppedOnRollback(t *testing.T) {
	ctx := postCommitTestContext()
	publisher := &postCommitRecordingPublisher{}
	refundSvc := NewRefundService(ServiceParams{
		Logger:           logger.NewNoopLogger(),
		WebhookPublisher: publisher,
	}).(*refundService)

	txCtx := postCommitTxContext(ctx)

	refundSvc.publishSystemEvent(txCtx, types.WebhookEventRefundFailed, "ref_post_commit_rollback")

	types.DiscardPostCommitHooks(txCtx)

	assert.Empty(t, publisher.published(), "a rolled-back transition must not emit a webhook")
	assert.Empty(t, publisher.publishContexts())
}

// ReconcilePaymentStatus runs inside the settlement transaction and dispatches
// mark-paid before the commit; the workflows must not start until it lands.
func TestDispatchMarkPaidQueuedUntilCommit(t *testing.T) {
	ctx := postCommitTestContext()
	repo := &dispatchRecordingConnectionRepo{}
	processor := NewPaymentProcessorService(ServiceParams{
		Logger:         logger.NewNoopLogger(),
		ConnectionRepo: repo,
	})

	txCtx := postCommitTxContext(ctx)

	processor.DispatchMarkPaid(txCtx, "inv_dispatch")
	assert.Empty(t, repo.callContexts(), "the dispatch must not run before the commit")

	types.RunPostCommitHooks(txCtx)

	contexts := repo.callContexts()
	require.Len(t, contexts, 1, "the committed transaction dispatches once")
	assert.Nil(t, contexts[0].Value(types.CtxDBTransaction),
		"the queued dispatch must run without the committed transaction")

	// The commit drains the hooks; a later drain must not dispatch again.
	types.RunPostCommitHooks(txCtx)
	assert.Len(t, repo.callContexts(), 1)
}

func TestDispatchMarkPaidDroppedOnRollback(t *testing.T) {
	ctx := postCommitTestContext()
	repo := &dispatchRecordingConnectionRepo{}
	processor := NewPaymentProcessorService(ServiceParams{
		Logger:         logger.NewNoopLogger(),
		ConnectionRepo: repo,
	})

	txCtx := postCommitTxContext(ctx)

	processor.DispatchMarkPaid(txCtx, "inv_dispatch_rollback")

	types.DiscardPostCommitHooks(txCtx)

	assert.Empty(t, repo.callContexts(), "a rolled-back transaction must not dispatch mark-paid")
}

// A purchased-credit invoice paid through the settlement path completes its
// wallet transaction inside the same transaction, so its webhook must wait too.
func TestWalletTransactionWebhookQueuedUntilCommit(t *testing.T) {
	ctx := postCommitTestContext()
	publisher := &postCommitRecordingPublisher{}
	walletSvc := NewWalletService(ServiceParams{
		Logger:           logger.NewNoopLogger(),
		WebhookPublisher: publisher,
	}).(*walletService)

	txCtx := postCommitTxContext(ctx)

	walletSvc.publishInternalTransactionWebhookEvent(txCtx, types.WebhookEventWalletTransactionUpdated, "wtx_post_commit")

	assert.Empty(t, publisher.published(), "no wallet webhook may be published before the commit")

	types.RunPostCommitHooks(txCtx)

	events := publisher.published()
	require.Len(t, events, 1, "the committed transition publishes once")
	assert.Equal(t, types.WebhookEventWalletTransactionUpdated, events[0].EventName)
	assert.Equal(t, types.SystemEntityTypeWallet, events[0].EntityType)
	assert.Equal(t, "wtx_post_commit", events[0].EntityID)

	var payload webhookDto.InternalTransactionEvent
	require.NoError(t, json.Unmarshal(events[0].Payload, &payload))
	assert.Equal(t, "wtx_post_commit", payload.TransactionID)
	assert.Equal(t, postCommitTestTenant, payload.TenantID)

	contexts := publisher.publishContexts()
	require.Len(t, contexts, 1)
	assert.Nil(t, contexts[0].Value(types.CtxDBTransaction),
		"the queued publish must run without the committed transaction")
}

func TestWalletTransactionWebhookDroppedOnRollback(t *testing.T) {
	ctx := postCommitTestContext()
	publisher := &postCommitRecordingPublisher{}
	walletSvc := NewWalletService(ServiceParams{
		Logger:           logger.NewNoopLogger(),
		WebhookPublisher: publisher,
	}).(*walletService)

	txCtx := postCommitTxContext(ctx)

	walletSvc.publishInternalTransactionWebhookEvent(txCtx, types.WebhookEventWalletTransactionCreated, "wtx_post_commit_rollback")

	types.DiscardPostCommitHooks(txCtx)

	assert.Empty(t, publisher.published(), "a rolled-back transition must not emit a webhook")
	assert.Empty(t, publisher.publishContexts())
}

// The settlement transaction activates subscriptions through
// HandleSubscriptionActivatingInvoicePaid, which publishes
// subscription.activated on the transaction context.
func TestSubscriptionPublishSystemEventQueuedUntilCommit(t *testing.T) {
	ctx := postCommitTestContext()
	publisher := &postCommitRecordingPublisher{}
	subSvc := NewSubscriptionService(ServiceParams{
		Logger:           logger.NewNoopLogger(),
		WebhookPublisher: publisher,
	}).(*subscriptionService)

	txCtx := postCommitTxContext(ctx)

	subSvc.publishSystemEvent(txCtx, types.WebhookEventSubscriptionActivated, "sub_post_commit")

	assert.Empty(t, publisher.published(), "no subscription webhook may be published before the commit")

	types.RunPostCommitHooks(txCtx)

	events := publisher.published()
	require.Len(t, events, 1, "the committed transition publishes once")
	assert.Equal(t, types.WebhookEventSubscriptionActivated, events[0].EventName)
	assert.Equal(t, types.SystemEntityTypeSubscription, events[0].EntityType)
	assert.Equal(t, "sub_post_commit", events[0].EntityID)

	var payload webhookDto.InternalSubscriptionEvent
	require.NoError(t, json.Unmarshal(events[0].Payload, &payload))
	assert.Equal(t, "sub_post_commit", payload.SubscriptionID)
	assert.Equal(t, postCommitTestTenant, payload.TenantID)

	contexts := publisher.publishContexts()
	require.Len(t, contexts, 1)
	assert.Nil(t, contexts[0].Value(types.CtxDBTransaction),
		"the queued publish must run without the committed transaction")
}

func TestSubscriptionPublishSystemEventDroppedOnRollback(t *testing.T) {
	ctx := postCommitTestContext()
	publisher := &postCommitRecordingPublisher{}
	subSvc := NewSubscriptionService(ServiceParams{
		Logger:           logger.NewNoopLogger(),
		WebhookPublisher: publisher,
	}).(*subscriptionService)

	txCtx := postCommitTxContext(ctx)

	subSvc.publishSystemEvent(txCtx, types.WebhookEventSubscriptionCancelled, "sub_post_commit_rollback")

	types.DiscardPostCommitHooks(txCtx)

	assert.Empty(t, publisher.published(), "a rolled-back transition must not emit a webhook")
	assert.Empty(t, publisher.publishContexts())
}
