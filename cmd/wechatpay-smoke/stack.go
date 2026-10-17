// Command wechatpay-smoke is the local acceptance harness for the WeChat Pay
// connector. It boots the real HTTP router, real services, real domain stores
// (in-memory, same testutil implementations the test suites use) and the real
// official WeChat Pay SDK, pointed at an isolated, signed simulation of the
// WeChat Pay APIv3 gateway in cmd/wechatpay-smoke/wechatsim. No request leaves
// the machine, no real merchant credential exists anywhere in this process:
// the fixture keys are generated per run and are obviously fake.
//
//	go run ./cmd/wechatpay-smoke
//
// exits non-zero if any assertion of the payment/refund lifecycle fails.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/flexprice/flexprice/internal/auth"
	"github.com/flexprice/flexprice/internal/cache"
	"github.com/flexprice/flexprice/internal/config"
	"github.com/flexprice/flexprice/internal/domain/connection"
	"github.com/flexprice/flexprice/internal/domain/customer"
	"github.com/flexprice/flexprice/internal/domain/entityintegrationmapping"
	envdomain "github.com/flexprice/flexprice/internal/domain/environment"
	"github.com/flexprice/flexprice/internal/domain/proration"
	tenantdomain "github.com/flexprice/flexprice/internal/domain/tenant"
	userdomain "github.com/flexprice/flexprice/internal/domain/user"
	"github.com/flexprice/flexprice/internal/ee/service"
	"github.com/flexprice/flexprice/internal/integration"
	"github.com/flexprice/flexprice/internal/integration/wechatpay"
	"github.com/flexprice/flexprice/internal/logger"
	"github.com/flexprice/flexprice/internal/security"
	"github.com/flexprice/flexprice/internal/testutil"
	"github.com/flexprice/flexprice/internal/tracing"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/flexprice/flexprice/internal/validator"
	webhookPayload "github.com/flexprice/flexprice/internal/webhook/payload"

	wechatsim "github.com/flexprice/flexprice/cmd/wechatpay-smoke/wechatsim"
)

const (
	smokeTenantID = "tenant_smoke_wechatpay"
	smokeUserID   = "usr_smoke_wechatpay"
	smokeEnvID    = "env_smoke_wechatpay"
	smokeAPIKey   = "sk_smoke_wechatpay_local_only_do_not_use"
)

// storeBundle carries every in-memory repository the canonical service test
// suite wires, so any production service constructed below behaves exactly as
// it does under the repository's own test fixtures.
type storeBundle struct {
	tenant                   *testutil.InMemoryTenantStore
	environment              *testutil.InMemoryEnvironmentStore
	user                     *testutil.InMemoryUserStore
	auth                     *testutil.InMemoryAuthRepository
	secret                   *testutil.InMemorySecretStore
	customer                 *testutil.InMemoryCustomerStore
	invoice                  *testutil.InMemoryInvoiceStore
	invoiceLineItem          *testutil.InMemoryInvoiceLineItemStore
	payment                  *testutil.InMemoryPaymentStore
	refund                   *testutil.InMemoryRefundStore
	wallet                   *testutil.InMemoryWalletStore
	connection               *testutil.InMemoryConnectionStore
	entityIntegrationMapping entityintegrationmapping.Repository
	creditNote               *testutil.InMemoryCreditNoteStore
	creditNoteLineItem       *testutil.InMemoryCreditNoteLineItemStore
	taxRate                  *testutil.InMemoryTaxRateStore
	taxApplied               *testutil.InMemoryTaxAppliedStore
	taxAssociation           *testutil.InMemoryTaxAssociationStore
	fxRate                   *testutil.InMemoryFXRateStore
	coupon                   *testutil.InMemoryCouponStore
	couponAssociation        *testutil.InMemoryCouponAssociationStore
	couponApplication        *testutil.InMemoryCouponApplicationStore
	settings                 *testutil.InMemorySettingsStore
	subscription             *testutil.InMemorySubscriptionStore
	subscriptionLineItem     *testutil.InMemorySubscriptionLineItemStore
	subscriptionPhase        *testutil.InMemorySubscriptionPhaseStore
	subscriptionSchedule     *testutil.InMemorySubscriptionScheduleStore
	plan                     *testutil.InMemoryPlanStore
	price                    *testutil.InMemoryPriceStore
	priceUnit                *testutil.InMemoryPriceUnitStore
	meter                    *testutil.InMemoryMeterStore
	feature                  *testutil.InMemoryFeatureStore
	entitlement              *testutil.InMemoryEntitlementStore
	entitlementGrant         *testutil.InMemoryEntitlementGrantStore
	creditGrant              *testutil.InMemoryCreditGrantStore
	creditGrantApplication   *testutil.InMemoryCreditGrantApplicationStore
	addon                    *testutil.InMemoryAddonStore
	addonAssociation         *testutil.InMemoryAddonAssociationStore
	group                    *testutil.InMemoryGroupStore
	task                     *testutil.InMemoryTaskStore
	event                    *testutil.InMemoryEventStore
	checkoutSession          *testutil.InMemoryCheckoutSessionStore
	usageRecord              *testutil.InMemoryUsageRecordStore
	analyticsView            *testutil.InMemoryAnalyticsViewStore
	revenueFact              *testutil.InMemoryRevenueFactStore
	meterUsage               *testutil.InMemoryMeterUsageStore
	planPriceSync            *testutil.InMemoryPlanPriceSyncStore
}

func buildStores() *storeBundle {
	subStore := testutil.NewInMemorySubscriptionStore()
	lineItemStore := testutil.NewInMemorySubscriptionLineItemStore()
	subStore.SetLineItemStore(lineItemStore)

	invoiceLineItemStore := testutil.NewInMemoryInvoiceLineItemStore()
	invoiceStore := testutil.NewInMemoryInvoiceStore()
	invoiceStore.SetLineItemStore(invoiceLineItemStore)
	subStore.SetInvoiceStore(invoiceStore)

	priceStore := testutil.NewInMemoryPriceStore()
	couponStore := testutil.NewInMemoryCouponStore()

	return &storeBundle{
		tenant:                   testutil.NewInMemoryTenantStore(),
		environment:              testutil.NewInMemoryEnvironmentStore(),
		user:                     testutil.NewInMemoryUserStore(),
		auth:                     testutil.NewInMemoryAuthRepository(),
		secret:                   testutil.NewInMemorySecretStore(),
		customer:                 testutil.NewInMemoryCustomerStore(),
		invoice:                  invoiceStore,
		invoiceLineItem:          invoiceLineItemStore,
		payment:                  testutil.NewInMemoryPaymentStore(),
		refund:                   testutil.NewInMemoryRefundStore(),
		wallet:                   testutil.NewInMemoryWalletStore(),
		connection:               testutil.NewInMemoryConnectionStore(),
		entityIntegrationMapping: testutil.NewInMemoryEntityIntegrationMappingStore(),
		creditNote:               testutil.NewInMemoryCreditNoteStore(),
		creditNoteLineItem:       testutil.NewInMemoryCreditNoteLineItemStore(),
		taxRate:                  testutil.NewInMemoryTaxRateStore(),
		taxApplied:               testutil.NewInMemoryTaxAppliedStore(),
		taxAssociation:           testutil.NewInMemoryTaxAssociationStore(),
		fxRate:                   testutil.NewInMemoryFXRateStore(),
		coupon:                   couponStore,
		couponAssociation:        testutil.NewInMemoryCouponAssociationStore(couponStore),
		couponApplication:        testutil.NewInMemoryCouponApplicationStore(),
		settings:                 testutil.NewInMemorySettingsStore(),
		subscription:             subStore,
		subscriptionLineItem:     lineItemStore,
		subscriptionPhase:        testutil.NewInMemorySubscriptionPhaseStore(),
		subscriptionSchedule:     testutil.NewInMemorySubscriptionScheduleStore(),
		plan:                     testutil.NewInMemoryPlanStore(),
		price:                    priceStore,
		priceUnit:                testutil.NewInMemoryPriceUnitStore(),
		meter:                    testutil.NewInMemoryMeterStore(),
		feature:                  testutil.NewInMemoryFeatureStore(),
		entitlement:              testutil.NewInMemoryEntitlementStore(),
		entitlementGrant:         testutil.NewInMemoryEntitlementGrantStore(),
		creditGrant:              testutil.NewInMemoryCreditGrantStore(),
		creditGrantApplication:   testutil.NewInMemoryCreditGrantApplicationStore(),
		addon:                    testutil.NewInMemoryAddonStore(),
		addonAssociation:         testutil.NewInMemoryAddonAssociationStore(),
		group:                    testutil.NewInMemoryGroupStore(),
		task:                     testutil.NewInMemoryTaskStore(),
		event:                    testutil.NewInMemoryEventStore(),
		checkoutSession:          testutil.NewInMemoryCheckoutSessionStore(),
		usageRecord:              testutil.NewInMemoryUsageRecordStore(),
		analyticsView:            testutil.NewInMemoryAnalyticsViewStore(),
		revenueFact:              testutil.NewInMemoryRevenueFactStore(),
		meterUsage:               testutil.NewInMemoryMeterUsageStore(),
		planPriceSync:            testutil.NewInMemoryPlanPriceSyncStore(priceStore, subStore, lineItemStore),
	}
}

// serviceBundle holds the real service implementations shared by the router
// and by assertion helpers.
type serviceBundle struct {
	invoice            service.InvoiceService
	creditNote         service.CreditNoteService
	customer           service.CustomerService
	payment            service.PaymentService
	processor          service.PaymentProcessorService
	refund             service.RefundService
	wallet             service.WalletService
	wechatPay          service.WechatPayService
	connection         service.ConnectionService
	integrationSync    service.IntegrationSyncService
	integrationMapping service.EntityIntegrationMappingService
}

// smokeStack is the fully booted application under test.
type smokeStack struct {
	cfg      *config.Configuration
	log      *logger.Logger
	enc      security.EncryptionService
	stores   *storeBundle
	factory  *integration.Factory
	services *serviceBundle

	fixture *wechatsim.Fixture
	sim     *wechatsim.Server
	gateway wechatpay.Gateway

	baseURL string
	client  *httpClient
	stop    func()

	webhooks *capturingPublisher
	receiver *isolatedReceiver
}

// bootSmokeStack constructs the full local stack. It returns an error rather
// than exiting so the caller can print one deterministic failure block.
func bootSmokeStack() (*smokeStack, error) {
	validator.NewValidator()

	fixture, err := wechatsim.NewFixture()
	if err != nil {
		return nil, fmt.Errorf("create fake merchant fixture: %w", err)
	}
	sim := wechatsim.NewServer(fixture)

	cfg := smokeConfig()
	log, err := logger.NewLogger(cfg)
	if err != nil {
		return nil, fmt.Errorf("create logger: %w", err)
	}

	enc, err := security.NewEncryptionService(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("create encryption service: %w", err)
	}

	stores := buildStores()

	gateway := wechatpay.NewClientWithHTTPClient(stores.connection, enc, log, sim.HTTPClient())

	factory := integration.NewFactory(
		cfg,
		log,
		stores.connection,
		stores.customer,
		stores.subscription,
		stores.plan,
		stores.invoice,
		stores.payment,
		nil, // payment method repo — saved instruments are out of scope for wechatpay
		stores.price,
		stores.entityIntegrationMapping,
		stores.meter,
		stores.feature,
		enc,
		nil, // temporal service — not used by the wechatpay paths
		testutil.NewInMemoryRedisLocker(testutil.NewInMemoryRedis().(*testutil.InMemoryRedis)),
		stores.settings,
	)
	factory.SetWechatPayGateway(gateway)

	receiver := newIsolatedReceiver()
	// The payload builder needs the services the events resolve through, so the
	// publisher starts with a nil builder and is completed right after the
	// services exist — always before the router serves its first request.
	webhooks := newCapturingPublisher(nil, receiver)

	params := buildServiceParams(cfg, log, enc, stores, factory, webhooks)

	services := &serviceBundle{
		invoice:            service.NewInvoiceService(params),
		creditNote:         service.NewCreditNoteService(params),
		customer:           service.NewCustomerService(params),
		payment:            service.NewPaymentService(params),
		processor:          service.NewPaymentProcessorService(params),
		refund:             service.NewRefundService(params),
		wallet:             service.NewWalletService(params),
		wechatPay:          service.NewWechatPayService(params),
		connection:         service.NewConnectionService(params, enc),
		integrationSync:    service.NewIntegrationSyncService(params),
		integrationMapping: service.NewEntityIntegrationMappingService(params),
	}
	factory.SetServices(services.payment, services.invoice)

	// The payload builders resolve entities through the same services the
	// application uses, so the receiver sees real outbound payloads.
	webhooks.builder = webhookPayload.NewPayloadBuilderFactory(&webhookPayload.Services{
		InvoiceService:    services.invoice,
		PaymentService:    services.payment,
		RefundService:     services.refund,
		CustomerService:   services.customer,
		CreditNoteService: services.creditNote,
	})

	if err := seedFixtures(context.Background(), cfg, enc, stores, fixture); err != nil {
		return nil, err
	}

	router, err := buildRouter(cfg, log, stores, services)
	if err != nil {
		return nil, err
	}

	server := newLocalServer(router)

	return &smokeStack{
		cfg:      cfg,
		log:      log,
		enc:      enc,
		stores:   stores,
		factory:  factory,
		services: services,
		fixture:  fixture,
		sim:      sim,
		gateway:  gateway,
		baseURL:  server.URL,
		client:   newHTTPClient(server.URL, smokeAPIKey, smokeEnvID),
		stop: func() {
			server.Close()
			sim.Close()
			receiver.Close()
		},
		webhooks: webhooks,
		receiver: receiver,
	}, nil
}

func smokeConfig() *config.Configuration {
	return &config.Configuration{
		Logging: config.LoggingConfig{Level: types.LogLevelInfo},
		Secrets: config.SecretsConfig{EncryptionKey: testutil.NewEncryptionKey()},
		Auth: config.AuthConfig{
			Provider: types.AuthProviderFlexprice,
			Secret:   "local-smoke-secret-not-a-credential",
			APIKey: config.APIKeyConfig{
				Header: "x-api-key",
				Keys: map[string]config.APIKeyDetails{
					auth.HashAPIKey(smokeAPIKey): {
						TenantID: smokeTenantID,
						UserID:   smokeUserID,
						Name:     "wechatpay-smoke",
						IsActive: true,
					},
				},
			},
		},
		RBAC: config.RBACConfig{RolesConfigPath: "internal/config/rbac/roles.json"},
	}
}

func buildServiceParams(
	cfg *config.Configuration,
	log *logger.Logger,
	enc security.EncryptionService,
	stores *storeBundle,
	factory *integration.Factory,
	webhooks *capturingPublisher,
) service.ServiceParams {
	redisCache := testutil.NewInMemoryRedis()
	return service.ServiceParams{
		Logger:        log,
		Config:        cfg,
		DB:            testutil.NewMockPostgresClient(log),
		PDFGenerator:  testutil.NewMockPDFGenerator(log),
		TracingSvc:    tracing.NewService(cfg, log),
		InMemoryCache: cache.NewInMemoryCache(),
		RedisCache:    redisCache,
		Locker:        testutil.NewInMemoryRedisLocker(redisCache.(*testutil.InMemoryRedis)),

		AuthRepo:                     stores.auth,
		UserRepo:                     stores.user,
		EventRepo:                    stores.event,
		MeterUsageRepo:               stores.meterUsage,
		MeterRepo:                    stores.meter,
		PriceRepo:                    stores.price,
		PriceUnitRepo:                stores.priceUnit,
		CustomerRepo:                 stores.customer,
		PlanRepo:                     stores.plan,
		SubRepo:                      stores.subscription,
		SubscriptionLineItemRepo:     stores.subscriptionLineItem,
		SubscriptionPhaseRepo:        stores.subscriptionPhase,
		SubScheduleRepo:              stores.subscriptionSchedule,
		WalletRepo:                   stores.wallet,
		TenantRepo:                   stores.tenant,
		InvoiceRepo:                  stores.invoice,
		InvoiceLineItemRepo:          stores.invoiceLineItem,
		FeatureRepo:                  stores.feature,
		EntitlementRepo:              stores.entitlement,
		EntitlementGrantRepo:         stores.entitlementGrant,
		PaymentRepo:                  stores.payment,
		RefundRepo:                   stores.refund,
		SecretRepo:                   stores.secret,
		EnvironmentRepo:              stores.environment,
		TaskRepo:                     stores.task,
		CreditGrantRepo:              stores.creditGrant,
		CreditNoteRepo:               stores.creditNote,
		CreditNoteLineItemRepo:       stores.creditNoteLineItem,
		CreditGrantApplicationRepo:   stores.creditGrantApplication,
		TaxRateRepo:                  stores.taxRate,
		FXRateRepo:                   stores.fxRate,
		TaxAssociationRepo:           stores.taxAssociation,
		TaxAppliedRepo:               stores.taxApplied,
		CouponRepo:                   stores.coupon,
		CouponAssociationRepo:        stores.couponAssociation,
		CouponApplicationRepo:        stores.couponApplication,
		AddonRepo:                    stores.addon,
		AddonAssociationRepo:         stores.addonAssociation,
		ConnectionRepo:               stores.connection,
		EntityIntegrationMappingRepo: stores.entityIntegrationMapping,
		UsageRecordRepo:              stores.usageRecord,
		SettingsRepo:                 stores.settings,
		GroupRepo:                    stores.group,
		PlanPriceSyncRepo:            stores.planPriceSync,
		CheckoutSessionRepo:          stores.checkoutSession,
		AnalyticsViewRepo:            stores.analyticsView,
		RevenueFactRepo:              stores.revenueFact,

		EventPublisher:      testutil.NewInMemoryEventPublisher(stores.event),
		WebhookPublisher:    webhooks,
		ProrationCalculator: proration.NewCalculator(log),
		IntegrationFactory:  factory,
		EncryptionService:   enc,
		StorageResolver:     noStorageResolver{},
	}
}

// seedFixtures writes the tenant, environment, user, customer and the fake
// merchant connection the whole smoke run operates on.
func seedFixtures(ctx context.Context, cfg *config.Configuration, enc security.EncryptionService, stores *storeBundle, fixture *wechatsim.Fixture) error {
	if err := stores.tenant.Create(ctx, &tenantdomain.Tenant{
		ID:             smokeTenantID,
		Name:           "WeChat Pay smoke tenant",
		Status:         types.StatusPublished,
		InternalStatus: types.TenantInternalStatusActive,
	}); err != nil {
		return fmt.Errorf("seed tenant: %w", err)
	}
	if err := stores.environment.Create(ctx, &envdomain.Environment{
		ID:   smokeEnvID,
		Name: "WeChat Pay smoke environment",
		Type: types.EnvironmentDevelopment,
		BaseModel: types.BaseModel{
			TenantID: smokeTenantID,
			Status:   types.StatusPublished,
		},
	}); err != nil {
		return fmt.Errorf("seed environment: %w", err)
	}
	if err := stores.user.Create(ctx, &userdomain.User{
		ID:    smokeUserID,
		Email: "wechatpay-smoke@example.com",
		Type:  types.UserTypeUser,
		Roles: []string{types.RoleSuperAdmin.String()},
		BaseModel: types.BaseModel{
			TenantID: smokeTenantID,
			Status:   types.StatusPublished,
		},
	}); err != nil {
		return fmt.Errorf("seed user: %w", err)
	}
	if err := stores.customer.Create(ctx, &customer.Customer{
		ID:         "cust_smoke_wechatpay",
		ExternalID: "ext_cust_smoke_wechatpay",
		Name:       "Smoke Customer",
		Email:      "smoke-customer@example.com",
		BaseModel: types.BaseModel{
			TenantID: smokeTenantID,
			Status:   types.StatusPublished,
		},
	}); err != nil {
		return fmt.Errorf("seed customer: %w", err)
	}

	encryptedPrivateKey, err := enc.Encrypt(fixture.MerchantPrivateKeyPEM())
	if err != nil {
		return fmt.Errorf("encrypt fake merchant private key: %w", err)
	}
	encryptedAPIv3Key, err := enc.Encrypt(wechatsim.FakeAPIv3Key)
	if err != nil {
		return fmt.Errorf("encrypt fake APIv3 key: %w", err)
	}

	conn := &connection.Connection{
		ID:           types.GenerateUUIDWithPrefix("conn"),
		Name:         "wechatpay smoke (fake merchant, no real credential)",
		ProviderType: types.SecretProviderWechatPay,
		EncryptedSecretData: types.ConnectionMetadata{
			WechatPay: &types.WechatPayConnectionMetadata{
				AppID:                           fixture.AppID,
				MerchantID:                      fixture.MerchantID,
				MerchantCertificateSerialNumber: fixture.MerchantSerialNo,
				MerchantPrivateKey:              encryptedPrivateKey,
				APIv3Key:                        encryptedAPIv3Key,
				PlatformPublicKey:               fixture.PlatformPublicKeyPEM(),
				PlatformPublicKeyID:             fixture.PlatformPublicKeyID,
				NotifyURL:                       "https://wechatpay-smoke.example.com/v1/webhooks/wechatpay",
			},
		},
		EnvironmentID: smokeEnvID,
		BaseModel: types.BaseModel{
			TenantID: smokeTenantID,
			Status:   types.StatusPublished,
		},
	}
	if err := stores.connection.Create(ctx, conn); err != nil {
		return fmt.Errorf("seed wechatpay connection: %w", err)
	}
	return nil
}

// requestContext builds a context carrying the smoke tenant/environment, used
// when the harness reads or stages stores directly.
func (s *smokeStack) requestContext() context.Context {
	ctx := context.Background()
	ctx = types.SetTenantID(ctx, smokeTenantID)
	ctx = types.SetEnvironmentID(ctx, smokeEnvID)
	return context.WithValue(ctx, types.CtxUserID, smokeUserID)
}

func main() {
	stack, err := bootSmokeStack()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wechatpay smoke boot failed: %v\n", err)
		os.Exit(1)
	}
	defer stack.stop()

	run := newSmokeRun(stack)
	if err := run.execute(); err != nil {
		fmt.Fprintf(os.Stderr, "\nwechatpay smoke FAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\nwechatpay smoke PASSED (run %s)\n", time.Now().UTC().Format(time.RFC3339))
}
