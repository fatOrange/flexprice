package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"

	"github.com/flexprice/flexprice/internal/api"
	v1 "github.com/flexprice/flexprice/internal/api/v1"
	"github.com/flexprice/flexprice/internal/config"
	domainIncomingWebhookEvent "github.com/flexprice/flexprice/internal/domain/incomingwebhookevent"
	"github.com/flexprice/flexprice/internal/ee/service"
	"github.com/flexprice/flexprice/internal/logger"
	"github.com/flexprice/flexprice/internal/rbac"
	"github.com/flexprice/flexprice/internal/storage"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/gin-gonic/gin"
)

// recordingWebhookRepo is the in-memory incoming-webhook audit repository the
// public webhook route uses. It mirrors the repository contract (persist every
// inbound provider callback) so the middleware exercises its real code path.
type recordingWebhookRepo struct {
	mu     sync.Mutex
	events []*domainIncomingWebhookEvent.IncomingWebhookEvent
}

func (r *recordingWebhookRepo) Create(_ context.Context, event *domainIncomingWebhookEvent.IncomingWebhookEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

func (r *recordingWebhookRepo) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// buildRouter mounts the production router with the real middleware chain and
// the handlers this smoke drives against the in-memory stores.
func buildRouter(cfg *config.Configuration, log *logger.Logger, stores *storeBundle, services *serviceBundle) (*gin.Engine, error) {
	rbacService, err := rbac.NewRBACService(cfg)
	if err != nil {
		return nil, err
	}

	secretService := service.NewSecretService(stores.secret, stores.user, cfg, log)
	envAccessService := service.NewEnvAccessService(cfg)
	tenantService := service.NewTenantService(service.ServiceParams{
		Logger:     log,
		Config:     cfg,
		TenantRepo: stores.tenant,
	})

	handlers := api.Handlers{
		Health:      v1.NewHealthHandler(log),
		Invoice:     v1.NewInvoiceHandler(services.invoice, cfg, log),
		Payment:     v1.NewPaymentHandler(services.payment, services.processor, log),
		Refund:      v1.NewRefundHandler(services.refund, log),
		CreditNote:  v1.NewCreditNoteHandler(services.creditNote, log),
		WechatPay:   v1.NewWechatPayHandler(services.wechatPay, log),
		Integration: v1.NewIntegrationHandler(services.integrationSync, services.integrationMapping, services.connection, log),
	}

	return api.NewRouter(
		handlers,
		cfg,
		log,
		secretService,
		envAccessService,
		rbacService,
		tenantService,
		&recordingWebhookRepo{},
		stores.environment,
		stores.user,
	), nil
}

func newLocalServer(engine *gin.Engine) *httptest.Server {
	return httptest.NewServer(engine)
}

// noStorageResolver satisfies the storage.Resolver contract with explicit
// errors. The smoke run never uploads invoice PDFs; the invoice payload builder
// falls back to the invoice's own pdf url, and anything that would genuinely
// need object storage reports an error instead of panicking on a nil resolver.
type noStorageResolver struct{}

func (noStorageResolver) ForPlatform(context.Context, storage.Purpose) (storage.Storage, error) {
	return nil, errNoObjectStorage
}

func (noStorageResolver) ForConnection(context.Context, string) (storage.Storage, error) {
	return nil, errNoObjectStorage
}

func (noStorageResolver) ForConnectionExport(context.Context, string, *types.S3JobConfig) (storage.Storage, error) {
	return nil, errNoObjectStorage
}

func (noStorageResolver) Provider() storage.Provider { return storage.Provider("none") }

func (noStorageResolver) BucketConfigFor(storage.Purpose) (config.BucketConfig, error) {
	return config.BucketConfig{}, errNoObjectStorage
}

var errNoObjectStorage = fmt.Errorf("wechatpay smoke: object storage is not configured")
