package v1

import (
	"io"
	"net/http"

	"github.com/flexprice/flexprice/internal/ee/service"
	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/logger"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/gin-gonic/gin"
)

// wechatPayCallbackMaxBodyBytes bounds the raw callback body. A signed WeChat
// Pay notification is a few kilobytes; anything larger is not a notification we
// could have produced and is refused before verification.
const wechatPayCallbackMaxBodyBytes = 1 << 20

// WechatPayHandler is the public contract for the signed WeChat Pay callback
// endpoint. The implementation is private; consumers receive it from
// NewWechatPayHandler.
type WechatPayHandler interface {
	HandleCallback(c *gin.Context)
}

// wechatPayHandler serves the public signed WeChat Pay callback endpoint. It
// carries no business logic: it scopes the request to the tenant and
// environment in the URL, bounds the raw body, and delegates verification plus
// mutation to the service.
type wechatPayHandler struct {
	service service.WechatPayService
	log     *logger.Logger
}

func NewWechatPayHandler(service service.WechatPayService, log *logger.Logger) WechatPayHandler {
	return &wechatPayHandler{service: service, log: log}
}

// HandleCallback handles POST /v1/webhooks/wechatpay/:tenant_id/:environment_id.
// Success is an empty 200: the provider needs only the status code to stop
// retrying. Any error is answered with a non-2xx status so WeChat Pay delivers
// the notification again — a callback is never acknowledged before its business
// effect is durably recorded.
func (h *wechatPayHandler) HandleCallback(c *gin.Context) {
	tenantID := c.Param("tenant_id")
	environmentID := c.Param("environment_id")
	if tenantID == "" || environmentID == "" {
		h.log.Error(c.Request.Context(), "wechatpay callback missing route scope",
			"error", "missing_tenant_or_environment",
			"tenant_id", tenantID,
			"environment_id", environmentID)
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, wechatPayCallbackMaxBodyBytes+1))
	if err != nil {
		h.log.Error(c.Request.Context(), "failed to read wechatpay callback body",
			"error", err,
			"tenant_id", tenantID,
			"environment_id", environmentID)
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if len(body) > wechatPayCallbackMaxBodyBytes {
		h.log.Error(c.Request.Context(), "wechatpay callback body exceeds limit",
			"error", "body_too_large",
			"limit_bytes", wechatPayCallbackMaxBodyBytes,
			"tenant_id", tenantID,
			"environment_id", environmentID)
		c.AbortWithStatus(http.StatusRequestEntityTooLarge)
		return
	}

	ctx := types.SetTenantID(c.Request.Context(), tenantID)
	ctx = types.SetEnvironmentID(ctx, environmentID)

	if err := h.service.HandleCallback(ctx, c.Request.Header, body); err != nil {
		// Validation failures (bad signature, wrong merchant, amount mismatch)
		// are terminal rejections; everything else may be transient and is a 500.
		// Both are non-2xx, which is what makes the provider retry.
		status := http.StatusInternalServerError
		if ierr.IsValidation(err) {
			status = http.StatusBadRequest
		}
		h.log.Error(ctx, "wechatpay callback rejected",
			"error", err,
			"tenant_id", tenantID,
			"environment_id", environmentID)
		c.AbortWithStatus(status)
		return
	}

	c.Status(http.StatusOK)
}
