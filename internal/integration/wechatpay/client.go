package wechatpay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/flexprice/flexprice/internal/domain/connection"
	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/interfaces"
	"github.com/flexprice/flexprice/internal/logger"
	"github.com/flexprice/flexprice/internal/security"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/validators"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/verifiers"
	"github.com/wechatpay-apiv3/wechatpay-go/core/notify"
	"github.com/wechatpay-apiv3/wechatpay-go/core/option"
)

// httpTimeout bounds every WeChat Pay API call. It is the hard ceiling when the
// caller's context carries no deadline of its own.
const httpTimeout = 30 * time.Second

// maxProviderResponseBodyBytes bounds the error body this integration is
// willing to verify signatures over. WeChat Pay error bodies are a few hundred
// bytes; anything larger is rejected unverified instead of fed to crypto.
const maxProviderResponseBodyBytes = 1 << 20

// WeChat Pay APIv3 error codes this integration translates into sentinel
// errors. Everything else surfaces as a generic provider error.
const (
	apiCodeOrderNotFound    = "ORDER_NOT_EXIST"
	apiCodeOrderPaid        = "ORDERPAID"
	apiCodeOrderClosed      = "ORDER_CLOSED"
	apiCodeResourceNotFound = "RESOURCE_NOT_EXISTS"
)

// errOrderClosed is an internal signal that a verified close-order response
// reported the order already closed. It never leaves the package: CloseOrder
// treats it as success, every other caller would see it as a plain error.
var errOrderClosed = errors.New("wechatpay: order already closed")

// Gateway is the WeChat Pay APIv3 client surface. Requests are signed with the
// merchant private key and responses are verified against the WeChat Pay
// platform public key by the official SDK; callbacks are verified and decrypted
// by the same code path. Implementations resolve the tenant and environment
// from the context and reload the connection on every call.
type Gateway interface {
	// CreatePayment pre-creates one WeChat Pay order for an invoice and returns
	// the customer-facing next action: a Native QR code payload or the signed
	// JSAPI parameters for the in-WeChat browser. options selects the channel
	// and defaults to Native. req.ExpiresAt is the persisted attempt expiry and
	// is sent as the provider time_expire; it is never derived from the retry
	// time.
	CreatePayment(ctx context.Context, req interfaces.CheckoutProviderRequest, options types.WechatPayOptions) (*interfaces.CheckoutProviderResponse, error)

	// GetTransaction returns the transaction for a merchant order number
	// (out_trade_no). ErrOrderNotFound means WeChat Pay has no such order.
	GetTransaction(ctx context.Context, orderID string) (*Transaction, error)

	// GetTransactionByID returns the transaction for a WeChat Pay transaction
	// id. ErrOrderNotFound means WeChat Pay has no such transaction.
	GetTransactionByID(ctx context.Context, transactionID string) (*Transaction, error)

	// CloseOrder closes a merchant order so it can no longer be paid.
	// ErrOrderPaid means the order was paid instead; the caller must query and
	// settle it. Closing an already closed order succeeds.
	CloseOrder(ctx context.Context, orderID string) error

	// ParseNotification verifies the platform signature over the raw body,
	// decrypts the notification resource with the APIv3 key, and returns the
	// typed payload. The raw bytes are never parsed for business data before
	// verification.
	ParseNotification(ctx context.Context, headers http.Header, rawBody []byte) (*Notification, error)

	// GetRefund returns the refund for a merchant refund number
	// (out_refund_no). ErrRefundNotFound means WeChat Pay has no such refund.
	GetRefund(ctx context.Context, outRefundNo string) (*Refund, error)
}

// client is the concrete Gateway. It carries only the collaborators needed to
// resolve a connection; tenant and environment specific SDK clients are built
// during resolve and never cached.
type client struct {
	connectionRepo    connection.Repository
	encryptionService security.EncryptionService
	logger            *logger.Logger
	httpClient        *http.Client
}

// NewClient creates the WeChat Pay gateway backed by the official APIv3 SDK.
func NewClient(
	connectionRepo connection.Repository,
	encryptionService security.EncryptionService,
	logger *logger.Logger,
) Gateway {
	return NewClientWithHTTPClient(connectionRepo, encryptionService, logger, nil)
}

// NewClientWithHTTPClient is NewClient with a caller-supplied HTTP client, so
// isolated tests and the local smoke harness can direct the real SDK at a
// simulated gateway through a custom transport. Production always uses the
// default client; the official API endpoint is baked into the SDK and is not
// configurable.
func NewClientWithHTTPClient(
	connectionRepo connection.Repository,
	encryptionService security.EncryptionService,
	logger *logger.Logger,
	httpClient *http.Client,
) Gateway {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: httpTimeout}
	}
	return &client{
		connectionRepo:    connectionRepo,
		encryptionService: encryptionService,
		logger:            logger,
		httpClient:        httpClient,
	}
}

// resolvedConfig bundles the per-request connection configuration with the SDK
// client and notification handler built from it.
type resolvedConfig struct {
	config *connectionConfig
	sdk    *core.Client
	notify *notify.Handler
	// validator is the official platform public-key response validator, kept
	// alongside the SDK client because the SDK does not validate signatures on
	// non-2xx responses itself.
	validator auth.Validator
}

// resolve rebuilds the SDK client from the current connection. Connections are
// tenant/environment scoped and may rotate at any time, so every call resolves
// them afresh rather than caching credentials process-wide.
func (c *client) resolve(ctx context.Context) (*resolvedConfig, error) {
	cfg, err := c.loadConnectionConfig(ctx)
	if err != nil {
		return nil, err
	}

	sdkClient, err := core.NewClient(ctx,
		option.WithWechatPayPublicKeyAuthCipher(
			cfg.merchantID,
			cfg.merchantSerialNo,
			cfg.merchantPrivateKey,
			cfg.platformPublicKeyID,
			cfg.platformPublicKey,
		),
		option.WithHTTPClient(c.httpClient),
	)
	if err != nil {
		return nil, ierr.WithError(err).
			WithHint("The WeChat Pay client could not be initialised from the connection credentials").
			Mark(ierr.ErrInternal)
	}

	verifier := verifiers.NewSHA256WithRSAPubkeyVerifier(cfg.platformPublicKeyID, *cfg.platformPublicKey)
	handler, err := notify.NewRSANotifyHandler(cfg.apiV3Key, verifier)
	if err != nil {
		return nil, ierr.WithError(err).
			WithHint("The WeChat Pay notification handler could not be initialised; check the APIv3 key").
			Mark(ierr.ErrValidation)
	}

	return &resolvedConfig{
		config:    cfg,
		sdk:       sdkClient,
		notify:    handler,
		validator: validators.NewWechatPayResponseValidator(verifier),
	}, nil
}

// providerError translates an SDK error into an internal error.
//
// The SDK checks the HTTP status and unwraps the API error body before it
// validates the response signature, so a non-2xx body — including its error
// code — is untrusted by default. Every non-2xx response is therefore verified
// here with the same configured platform public-key validator the SDK uses for
// 2xx responses before any code is allowed to influence state. notFound is
// returned wrapped only when the verified body carries exactly notFoundCode,
// the official code for that resource; a signed 404 with any other code proves
// nothing and stays a generic, uncertain error. Anything unverified stays
// uncertain too.
func (c *client) providerError(ctx context.Context, res *resolvedConfig, operation string, err error, notFound error, notFoundCode string) error {
	var apiErr *core.APIError
	if !errors.As(err, &apiErr) {
		c.logger.Error(ctx, "wechatpay request failed", "error", err, "operation", operation)
		return ierr.WithError(err).
			WithHint("WeChat Pay could not be reached").
			WithReportableDetails(map[string]any{"operation": operation}).
			Mark(ierr.ErrHTTPClient)
	}

	if verifyErr := verifyAPIErrorResponse(ctx, res.validator, apiErr); verifyErr != nil {
		c.logger.Error(ctx, "wechatpay unverified error response rejected",
			"error", verifyErr,
			"operation", operation,
			"status_code", apiErr.StatusCode)
		return ierr.NewError("wechatpay error response could not be verified").
			WithHint("The WeChat Pay error response did not carry a valid platform signature; its error code is not trusted").
			WithReportableDetails(map[string]any{
				"operation":   operation,
				"status_code": apiErr.StatusCode,
			}).
			Mark(ierr.ErrHTTPClient)
	}

	switch {
	case notFound != nil && notFoundCode != "" && apiErr.Code == notFoundCode:
		return ierr.WithError(notFound).
			WithHint("WeChat Pay has no record for this merchant order or refund").
			WithReportableDetails(map[string]any{"operation": operation, "code": apiErr.Code}).
			Mark(ierr.ErrNotFound)
	case apiErr.Code == apiCodeOrderPaid:
		return ierr.WithError(ErrOrderPaid).
			WithHint("The order was already paid at WeChat Pay; query it and settle instead of closing").
			WithReportableDetails(map[string]any{"operation": operation, "code": apiErr.Code}).
			Mark(ierr.ErrValidation)
	case apiErr.Code == apiCodeOrderClosed:
		return errOrderClosed
	}

	c.logger.Error(ctx, "wechatpay API request failed",
		"error", err,
		"operation", operation,
		"status_code", apiErr.StatusCode,
		"code", apiErr.Code)
	return ierr.NewError("wechatpay API request failed").
		WithHint("WeChat Pay rejected the request").
		WithReportableDetails(map[string]any{
			"operation":   operation,
			"status_code": apiErr.StatusCode,
			"code":        apiErr.Code,
		}).
		Mark(ierr.ErrHTTPClient)
}

// verifyAPIErrorResponse verifies the platform signature over a non-2xx body
// with the official SDK validator. The body is taken from the already-read API
// error and bounded before any cryptography runs.
func verifyAPIErrorResponse(ctx context.Context, validator auth.Validator, apiErr *core.APIError) error {
	body := []byte(apiErr.Body)
	if len(body) > maxProviderResponseBodyBytes {
		return fmt.Errorf("error response body is %d bytes, over the %d byte verification bound", len(body), maxProviderResponseBodyBytes)
	}
	response := &http.Response{
		StatusCode: apiErr.StatusCode,
		Header:     apiErr.Header,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	return validator.Validate(ctx, response)
}
