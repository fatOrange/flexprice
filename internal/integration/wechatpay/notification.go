package wechatpay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	ierr "github.com/flexprice/flexprice/internal/errors"
)

// maxNotificationBodyBytes bounds the raw callback body accepted for
// verification. WeChat Pay notifications are a few kilobytes; anything larger
// is rejected before any cryptography runs.
const maxNotificationBodyBytes = 1 << 20

// notificationNonceLength is the AES-GCM nonce size WeChat Pay uses on the
// wire. The SDK hands the wire nonce straight to cipher.AEAD.Open, which
// panics on any other length, so callbacks are rejected here before the SDK
// ever sees them.
const notificationNonceLength = 12

// ParseNotification verifies the WeChat Pay platform signature over the raw
// bytes, decrypts the notification resource with the APIv3 key, and returns the
// typed payload. The envelope is only read to select the payload type and to
// reject a resource nonce the SDK cipher would panic on; no business data is
// taken from it before the signature verifies.
func (c *client) ParseNotification(ctx context.Context, headers http.Header, rawBody []byte) (*Notification, error) {
	if len(rawBody) == 0 {
		return nil, ierr.NewError("wechatpay notification body is empty").
			WithHint("Pass the exact bytes of the callback request body").
			Mark(ierr.ErrValidation)
	}
	if len(rawBody) > maxNotificationBodyBytes {
		return nil, ierr.NewError("wechatpay notification body is too large").
			WithHint("WeChat Pay notifications are a few kilobytes; reject anything larger").
			WithReportableDetails(map[string]any{"size": len(rawBody)}).
			Mark(ierr.ErrValidation)
	}

	res, err := c.resolve(ctx)
	if err != nil {
		return nil, err
	}

	var envelope struct {
		EventType string `json:"event_type"`
		Resource  struct {
			OriginalType string `json:"original_type"`
			Nonce        string `json:"nonce"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		return nil, ierr.WithError(err).
			WithHint("The WeChat Pay notification body is not valid JSON").
			Mark(ierr.ErrValidation)
	}
	if len(envelope.Resource.Nonce) != notificationNonceLength {
		return nil, ierr.NewError("wechatpay notification nonce is invalid").
			WithHint("The WeChat Pay notification resource nonce must be exactly 12 bytes").
			WithReportableDetails(map[string]any{
				"event_type":   envelope.EventType,
				"nonce_length": len(envelope.Resource.Nonce),
			}).
			Mark(ierr.ErrValidation)
	}

	content, err := notificationContent(envelope.EventType, envelope.Resource.OriginalType)
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/webhooks/wechatpay", bytes.NewReader(rawBody))
	if err != nil {
		return nil, ierr.WithError(err).
			WithHint("The WeChat Pay notification could not be prepared for verification").
			Mark(ierr.ErrInternal)
	}
	cloned := headers.Clone()
	if cloned == nil {
		cloned = http.Header{}
	}
	request.Header = cloned

	if _, err := res.notify.ParseNotifyRequest(ctx, request, content); err != nil {
		// The SDK error embeds the whole HTTP request — signature headers and
		// the encrypted body included — so it is never attached to the error
		// this method returns. Only the stable failure category is preserved;
		// the HTTP boundary owns logging the rejection once.
		reason := notificationFailureReason(err)
		return nil, ierr.NewError("wechatpay notification rejected: " + reason).
			WithHint("The WeChat Pay notification signature could not be verified or the resource could not be decrypted").
			WithReportableDetails(map[string]any{
				"event_type": envelope.EventType,
				"reason":     reason,
			}).
			Mark(ierr.ErrPermissionDenied)
	}

	notification := &Notification{EventType: envelope.EventType}
	switch payload := content.(type) {
	case *Transaction:
		notification.Transaction = payload
	case *Refund:
		notification.Refund = payload
	}
	return notification, nil
}

// notificationFailureReason classifies an SDK notify error into a stable,
// safe category. The SDK error itself is never propagated or logged: it embeds
// the full HTTP request, including signature headers and the encrypted body.
// Wording changes in the SDK only degrade the category to the generic default.
func notificationFailureReason(err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "decrypt error"):
		return "decryption_failed"
	case strings.Contains(message, "possible invalid notification"):
		return "algorithm_mismatch"
	case strings.Contains(message, "invalid notification"):
		return "signature_verification_failed"
	case strings.Contains(message, "parse request body error"):
		return "malformed_envelope"
	case strings.Contains(message, "unmarshal plaintext"):
		return "malformed_payload"
	default:
		return "verification_failed"
	}
}

// notificationContent returns the decrypt target for an event. Transaction and
// refund events decode into their typed structs; every other event type
// decrypts into a throwaway target so the caller can acknowledge it without
// acting on it. An event whose declared resource type contradicts its event
// type is rejected rather than guessed at.
func notificationContent(eventType, originalType string) (any, error) {
	isTransaction := originalType == resourceTypeTransaction || strings.HasPrefix(eventType, eventTypeTransactionPrefix)
	isRefund := originalType == resourceTypeRefund || strings.HasPrefix(eventType, eventTypeRefundPrefix)
	switch {
	case isTransaction && isRefund:
		return nil, ierr.NewError("wechatpay notification type is inconsistent").
			WithHint("The event type and resource type disagree; the notification cannot be trusted").
			WithReportableDetails(map[string]any{"event_type": eventType, "resource_type": originalType}).
			Mark(ierr.ErrValidation)
	case isTransaction:
		return new(Transaction), nil
	case isRefund:
		return new(Refund), nil
	default:
		return nil, nil
	}
}
