package v1

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/flexprice/flexprice/internal/api/dto"
	"github.com/flexprice/flexprice/internal/cache"
	"github.com/flexprice/flexprice/internal/config"
	"github.com/flexprice/flexprice/internal/domain/connection"
	"github.com/flexprice/flexprice/internal/domain/customer"
	"github.com/flexprice/flexprice/internal/domain/invoice"
	"github.com/flexprice/flexprice/internal/domain/payment"
	"github.com/flexprice/flexprice/internal/domain/refund"
	"github.com/flexprice/flexprice/internal/ee/service"
	"github.com/flexprice/flexprice/internal/integration"
	"github.com/flexprice/flexprice/internal/integration/wechatpay"
	"github.com/flexprice/flexprice/internal/interfaces"
	"github.com/flexprice/flexprice/internal/logger"
	"github.com/flexprice/flexprice/internal/security"
	"github.com/flexprice/flexprice/internal/testutil"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	wechatsim "github.com/flexprice/flexprice/cmd/wechatpay-smoke/wechatsim"
)

const (
	wpTenantID = "tenant_wp_verification"
	wpEnvID    = "env_wp_verification"
	wpCustomer = "cust_wp_verification"
)

// wechatPayTestStack wires the real WeChat Pay service, refund service and
// HTTP callback handler to in-memory domain stores and the isolated signed
// gateway simulation, with generated fake keys. It is the smallest faithful
// stack for exercising the public callback and query seams.
type wechatPayTestStack struct {
	t       *testing.T
	ctx     context.Context
	log     *logger.Logger
	cfg     *config.Configuration
	enc     security.EncryptionService
	fixture *wechatsim.Fixture
	sim     *wechatsim.Server
	gateway wechatpay.Gateway

	connectionRepo *testutil.InMemoryConnectionStore
	customerRepo   *testutil.InMemoryCustomerStore
	invoiceRepo    *testutil.InMemoryInvoiceStore
	lineItemRepo   *testutil.InMemoryInvoiceLineItemStore
	paymentRepo    *testutil.InMemoryPaymentStore
	refundRepo     *testutil.InMemoryRefundStore
	eventRepo      *testutil.InMemoryEventStore
	walletRepo     *testutil.InMemoryWalletStore

	params        service.ServiceParams
	wechatService service.WechatPayService
	refundService service.RefundService
}

func newWechatPayTestStack(t *testing.T) *wechatPayTestStack {
	t.Helper()

	fixture, err := wechatsim.NewFixture()
	require.NoError(t, err)
	sim := wechatsim.NewServer(fixture)
	t.Cleanup(sim.Close)

	cfg := &config.Configuration{
		Logging: config.LoggingConfig{Level: types.LogLevelError},
		Secrets: config.SecretsConfig{EncryptionKey: testutil.NewEncryptionKey()},
	}
	log, err := logger.NewLogger(cfg)
	require.NoError(t, err)
	enc, err := security.NewEncryptionService(cfg, log)
	require.NoError(t, err)

	invoiceRepo := testutil.NewInMemoryInvoiceStore()
	lineItemRepo := testutil.NewInMemoryInvoiceLineItemStore()
	invoiceRepo.SetLineItemStore(lineItemRepo)
	paymentRepo := testutil.NewInMemoryPaymentStore()
	refundRepo := testutil.NewInMemoryRefundStore()
	connectionRepo := testutil.NewInMemoryConnectionStore()
	customerRepo := testutil.NewInMemoryCustomerStore()
	eventRepo := testutil.NewInMemoryEventStore()
	walletRepo := testutil.NewInMemoryWalletStore()

	gateway := wechatpay.NewClientWithHTTPClient(connectionRepo, enc, log, sim.HTTPClient())
	factory := integration.NewFactory(
		cfg,
		log,
		connectionRepo,
		customerRepo,
		nil, // subscription repo
		nil, // plan repo
		invoiceRepo,
		paymentRepo,
		nil, // payment method repo
		nil, // price repo
		nil, // entity integration mapping repo
		nil, // meter repo
		nil, // feature repo
		enc,
		nil,
		testutil.NewInMemoryRedisLocker(testutil.NewInMemoryRedis().(*testutil.InMemoryRedis)),
		testutil.NewInMemorySettingsStore(),
	)
	factory.SetWechatPayGateway(gateway)

	redisCache := testutil.NewInMemoryRedis()
	params := service.ServiceParams{
		Logger:              log,
		Config:              cfg,
		DB:                  testutil.NewMockPostgresClient(log),
		InMemoryCache:       cache.NewInMemoryCache(),
		RedisCache:          redisCache,
		Locker:              testutil.NewInMemoryRedisLocker(redisCache.(*testutil.InMemoryRedis)),
		EventRepo:           eventRepo,
		CustomerRepo:        customerRepo,
		InvoiceRepo:         invoiceRepo,
		InvoiceLineItemRepo: lineItemRepo,
		PaymentRepo:         paymentRepo,
		RefundRepo:          refundRepo,
		ConnectionRepo:      connectionRepo,
		WalletRepo:          walletRepo,
		SettingsRepo:        testutil.NewInMemorySettingsStore(),
		EventPublisher:      testutil.NewInMemoryEventPublisher(eventRepo),
		WebhookPublisher:    testutil.NewInMemoryWebhookPublisher(),
		IntegrationFactory:  factory,
		EncryptionService:   enc,
	}

	ctx := context.Background()
	ctx = types.SetTenantID(ctx, wpTenantID)
	ctx = types.SetEnvironmentID(ctx, wpEnvID)

	stack := &wechatPayTestStack{
		t:              t,
		ctx:            ctx,
		log:            log,
		cfg:            cfg,
		enc:            enc,
		fixture:        fixture,
		sim:            sim,
		gateway:        gateway,
		connectionRepo: connectionRepo,
		customerRepo:   customerRepo,
		invoiceRepo:    invoiceRepo,
		lineItemRepo:   lineItemRepo,
		paymentRepo:    paymentRepo,
		refundRepo:     refundRepo,
		eventRepo:      eventRepo,
		walletRepo:     walletRepo,
		params:         params,
		wechatService:  service.NewWechatPayService(params),
		refundService:  service.NewRefundService(params),
	}
	stack.seedMerchant(t)
	return stack
}

// seedMerchant writes the fake merchant connection and a customer.
func (s *wechatPayTestStack) seedMerchant(t *testing.T) {
	t.Helper()

	encryptedPrivateKey, err := s.enc.Encrypt(s.fixture.MerchantPrivateKeyPEM())
	require.NoError(t, err)
	encryptedAPIv3Key, err := s.enc.Encrypt(wechatsim.FakeAPIv3Key)
	require.NoError(t, err)

	require.NoError(t, s.connectionRepo.Create(s.ctx, &connection.Connection{
		ID:           types.GenerateUUIDWithPrefix("conn"),
		Name:         "wechatpay test (fake merchant)",
		ProviderType: types.SecretProviderWechatPay,
		EncryptedSecretData: types.ConnectionMetadata{
			WechatPay: &types.WechatPayConnectionMetadata{
				AppID:                           s.fixture.AppID,
				MerchantID:                      s.fixture.MerchantID,
				MerchantCertificateSerialNumber: s.fixture.MerchantSerialNo,
				MerchantPrivateKey:              encryptedPrivateKey,
				APIv3Key:                        encryptedAPIv3Key,
				PlatformPublicKey:               s.fixture.PlatformPublicKeyPEM(),
				PlatformPublicKeyID:             s.fixture.PlatformPublicKeyID,
				NotifyURL:                       "https://wechatpay-verification.example.com/v1/webhooks/wechatpay",
			},
		},
		EnvironmentID: wpEnvID,
		BaseModel:     types.BaseModel{TenantID: wpTenantID, Status: types.StatusPublished},
	}))
	require.NoError(t, s.customerRepo.Create(s.ctx, &customer.Customer{
		ID:         wpCustomer,
		ExternalID: "ext_" + wpCustomer,
		Name:       "WeChat Pay verification customer",
		Email:      "wechatpay-verification@example.com",
		BaseModel:  types.BaseModel{TenantID: wpTenantID, Status: types.StatusPublished},
	}))
}

// seedFinalizedInvoice writes a finalized, unpaid CNY invoice for the amount.
func (s *wechatPayTestStack) seedFinalizedInvoice(t *testing.T, invoiceID, amount string) {
	t.Helper()
	due := decimal.RequireFromString(amount)
	require.NoError(t, s.invoiceRepo.Create(s.ctx, &invoice.Invoice{
		ID:              invoiceID,
		CustomerID:      wpCustomer,
		InvoiceType:     types.InvoiceTypeOneOff,
		InvoiceStatus:   types.InvoiceStatusFinalized,
		PaymentStatus:   types.PaymentStatusPending,
		Currency:        "cny",
		AmountDue:       due,
		Total:           due,
		Subtotal:        due,
		AmountPaid:      decimal.Zero,
		AmountRemaining: due,
		BaseModel:       types.BaseModel{TenantID: wpTenantID, Status: types.StatusPublished},
		EnvironmentID:   wpEnvID,
	}))
}

// seedOrderAndPayment creates a real WeChat Pay order through the SDK against
// the simulator and persists the bound local payment, mirroring what the
// application does at order creation.
func (s *wechatPayTestStack) seedOrderAndPayment(t *testing.T, paymentID, invoiceID, amount string) *payment.Payment {
	t.Helper()
	due := decimal.RequireFromString(amount)
	expiresAt := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)

	resp, err := s.gateway.CreatePayment(s.ctx, interfaces.CheckoutProviderRequest{
		InvoiceID:  invoiceID,
		CustomerID: wpCustomer,
		PaymentID:  paymentID,
		Amount:     due,
		Currency:   "CNY",
		ExpiresAt:  &expiresAt,
	}, types.WechatPayOptions{})
	require.NoError(t, err, "creating the simulated order through the real SDK must succeed")
	require.NotEmpty(t, resp.ProviderSessionID)

	row := &payment.Payment{
		ID:                paymentID,
		DestinationType:   types.PaymentDestinationTypeInvoice,
		DestinationID:     invoiceID,
		PaymentMethodType: types.PaymentMethodTypePaymentLink,
		PaymentGateway:    new(string(types.PaymentGatewayTypeWechatPay)),
		GatewayTrackingID: new(resp.ProviderSessionID),
		GatewayMetadata: types.Metadata{
			dto.GatewayMetadataWechatPayExpiresAt: expiresAt.Format(time.RFC3339),
		},
		Amount:        due,
		Currency:      "cny",
		PaymentStatus: types.PaymentStatusPending,
		TrackAttempts: true,
		EnvironmentID: wpEnvID,
		BaseModel:     types.BaseModel{TenantID: wpTenantID, Status: types.StatusPublished},
	}
	require.NoError(t, s.paymentRepo.Create(s.ctx, row))
	return row
}

// stageExpiry moves the persisted deadline of a payment into the past.
func (s *wechatPayTestStack) stageExpiry(t *testing.T, paymentID string) {
	t.Helper()
	row, err := s.paymentRepo.Get(s.ctx, paymentID)
	require.NoError(t, err)
	row.GatewayMetadata[dto.GatewayMetadataWechatPayExpiresAt] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	require.NoError(t, s.paymentRepo.Update(s.ctx, row))
}

// callbackRouter mounts the real callback handler on the production route path.
func (s *wechatPayTestStack) callbackRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/webhooks/wechatpay/:tenant_id/:environment_id", NewWechatPayHandler(s.wechatService, s.log).HandleCallback)
	return router
}

func (s *wechatPayTestStack) postCallback(router *gin.Engine, tenantID, envID string, headers http.Header, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/wechatpay/"+tenantID+"/"+envID, bytes.NewReader(body))
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// payOrder drives the simulated payer to settle the gateway order.
func (s *wechatPayTestStack) payOrder(orderID string) {
	if order := s.sim.Order(orderID); order != nil && order.TradeState != "SUCCESS" {
		s.sim.MarkPaid(orderID, time.Now())
	}
}

// signedTransaction builds a signed TRANSACTION.SUCCESS notification for a
// simulated order. Order fields default to the fixture merchant; pass overrides
// to forge specific mismatches.
func (s *wechatPayTestStack) signedTransaction(orderID string, amountFen int64, mchID, appID string) (http.Header, []byte) {
	if mchID == "" {
		mchID = s.fixture.MerchantID
	}
	if appID == "" {
		appID = s.fixture.AppID
	}
	plaintext := wechatsim.TransactionJSON(wechatsim.TransactionParams{
		AppID:         appID,
		MchID:         mchID,
		OutTradeNo:    orderID,
		TransactionID: orderTransactionID(s.sim.Order(orderID)),
		TradeState:    "SUCCESS",
		TradeType:     "NATIVE",
		SuccessTime:   time.Now().Format("2006-01-02T15:04:05+08:00"),
		TotalFen:      amountFen,
		Currency:      "CNY",
	})
	headers, body, err := s.fixture.SignedNotification("TRANSACTION.SUCCESS", "transaction", plaintext)
	require.NoError(s.t, err)
	return headers, body
}

// signedRefund builds a signed REFUND.SUCCESS notification.
func (s *wechatPayTestStack) signedRefund(outTradeNo, outRefundNo, refundID string, amountFen int64) (http.Header, []byte) {
	return s.signedRefundOutcome("REFUND.SUCCESS", "SUCCESS", outTradeNo, outRefundNo, refundID, amountFen)
}

// signedRefundOutcome builds a signed refund notification for any REFUND.*
// event and provider status (SUCCESS, ABNORMAL, CLOSED).
func (s *wechatPayTestStack) signedRefundOutcome(eventType, status, outTradeNo, outRefundNo, refundID string, amountFen int64) (http.Header, []byte) {
	plaintext := wechatsim.RefundJSON(wechatsim.RefundParams{
		OutTradeNo:    outTradeNo,
		TransactionID: orderTransactionID(s.sim.Order(outTradeNo)),
		OutRefundNo:   outRefundNo,
		RefundID:      refundID,
		Status:        status,
		SuccessTime:   time.Now().Format("2006-01-02T15:04:05+08:00"),
		RefundFen:     amountFen,
		TotalFen:      amountFen,
		Currency:      "CNY",
	})
	headers, body, err := s.fixture.SignedNotification(eventType, "refund", plaintext)
	require.NoError(s.t, err)
	return headers, body
}

// planAndSeedProcessingRefund issues a full refund through the real refund
// adapter against the simulator and persists the canonical PROCESSING row the
// provider callbacks and status queries resolve, returning both the row and the
// simulated provider state.
func (s *wechatPayTestStack) planAndSeedProcessingRefund(t *testing.T, paymentID, invoiceID, idempotencyKey string) (*refund.Refund, *wechatsim.RefundState) {
	t.Helper()

	paymentRow, err := s.paymentRepo.Get(s.ctx, paymentID)
	require.NoError(t, err)
	require.NotNil(t, paymentRow.GatewayPaymentID)

	adapter := wechatpay.NewRefundAdapter(s.gateway)
	full, err := adapter.RefundPayment(s.ctx, interfaces.RefundProviderRequest{
		GatewayPaymentID: *paymentRow.GatewayPaymentID,
		Amount:           paymentRow.Amount,
		Currency:         "CNY",
		IdempotencyKey:   idempotencyKey,
	})
	require.NoError(t, err)
	require.Equal(t, types.RefundStatusProcessing, full.Status, "WeChat Pay accepts refunds asynchronously")

	outRefundNo := wechatpay.MerchantRefundNo(idempotencyKey)
	state := s.sim.Refund(outRefundNo)
	require.NotNil(t, state, "the gateway must have seen the full refund create")

	row := &refund.Refund{
		ID:                      "ref_" + paymentID,
		PaymentID:               new(paymentID),
		InvoiceID:               invoiceID,
		PaymentGateway:          new(string(types.PaymentGatewayTypeWechatPay)),
		GatewayRefundID:         new(state.RefundID),
		Amount:                  paymentRow.Amount,
		Currency:                "cny",
		RefundStatus:            types.RefundStatusProcessing,
		RefundReason:            types.RefundReasonRequestedByCustomer,
		RefundDestination:       types.RefundDestinationGateway,
		IdempotencyKey:          idempotencyKey,
		GatewayIdempotencyToken: new(idempotencyKey),
		EnvironmentID:           wpEnvID,
		BaseModel:               types.BaseModel{TenantID: wpTenantID, Status: types.StatusPublished},
	}
	require.NoError(t, s.refundRepo.Create(s.ctx, row))
	return row, state
}

func orderTransactionID(order *wechatsim.OrderState) string {
	if order == nil {
		return ""
	}
	return order.TransactionID
}
