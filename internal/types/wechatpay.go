package types

import (
	"net"
	"net/url"
	"strings"

	ierr "github.com/flexprice/flexprice/internal/errors"
)

// WeChat Pay payment channels. Native renders a QR code the customer scans from
// the WeChat app; JSAPI is invoked inside the WeChat browser and needs the
// payer's openid. There is no H5, app, or mini-program channel.
const (
	WechatPayPaymentTypeNative = "native"
	WechatPayPaymentTypeJSAPI  = "jsapi"
)

// WechatPayOptions carries the WeChat Pay options for a single payment.
// An omitted payment_type means native, so a caller that only sends an amount
// gets the desktop QR flow.
type WechatPayOptions struct {
	PaymentType string `json:"payment_type,omitempty"`
	OpenID      string `json:"openid,omitempty"`
}

// Validate validates the requested channel and its required parameters.
func (o *WechatPayOptions) Validate() error {
	if o == nil {
		return nil
	}

	switch o.PaymentType {
	case "", WechatPayPaymentTypeNative:
		return nil
	case WechatPayPaymentTypeJSAPI:
		if o.OpenID == "" {
			return ierr.NewError("openid is required for jsapi payments").
				WithHint("Provide the payer's WeChat openid for a jsapi payment, or omit payment_type for a native QR payment").
				Mark(ierr.ErrValidation)
		}
		return nil
	default:
		return ierr.NewError("invalid wechatpay payment_type").
			WithHint("Allowed values: native, jsapi").
			WithReportableDetails(map[string]any{
				"allowed_values": []string{WechatPayPaymentTypeNative, WechatPayPaymentTypeJSAPI},
			}).
			Mark(ierr.ErrValidation)
	}
}

// EffectivePaymentType returns the channel the payment will use, defaulting to
// native when omitted. Safe on a nil receiver.
func (o *WechatPayOptions) EffectivePaymentType() string {
	if o == nil || o.PaymentType == "" {
		return WechatPayPaymentTypeNative
	}
	return o.PaymentType
}

// IsJSAPI reports whether the payment uses the in-WeChat-browser JSAPI channel.
// Safe on a nil receiver (false).
func (o *WechatPayOptions) IsJSAPI() bool {
	return o.EffectivePaymentType() == WechatPayPaymentTypeJSAPI
}

// WechatPayJSAPIParameters is the signed parameter set the WeChat browser needs
// to invoke WeixinJSBridge. Field names, camelCase JSON keys, and the string
// timestamp follow WeChat's wire format exactly; renaming or re-typing any of
// them breaks the call.
type WechatPayJSAPIParameters struct {
	AppID     string `json:"appId"`
	TimeStamp string `json:"timeStamp"`
	NonceStr  string `json:"nonceStr"`
	Package   string `json:"package"`
	SignType  string `json:"signType"`
	PaySign   string `json:"paySign"`
}

// WechatPayConnectionMetadata holds one WeChat Pay merchant connection. The wire
// keys are WeChat's own names (appid, mchid) because these values are copied
// verbatim from the merchant platform.
//
// MerchantPrivateKey and APIv3Key are encrypted at rest; the remaining fields
// are public configuration values stored in the clear.
type WechatPayConnectionMetadata struct {
	AppID                           string `json:"appid"`
	MerchantID                      string `json:"mchid"`
	MerchantCertificateSerialNumber string `json:"merchant_certificate_serial_number"`
	MerchantPrivateKey              string `json:"merchant_private_key"`
	APIv3Key                        string `json:"api_v3_key"`
	PlatformPublicKey               string `json:"platform_public_key"`
	PlatformPublicKeyID             string `json:"platform_public_key_id"`
	NotifyURL                       string `json:"notify_url"`
}

// Validate validates the WeChat Pay connection metadata.
//
// Secret material (merchant_private_key, api_v3_key) is only checked for
// presence, never for format: the same fields arrive as plaintext in create and
// update requests but are re-read as ciphertext from storage, and any format
// check would reject the stored form.
func (w *WechatPayConnectionMetadata) Validate() error {
	if w == nil {
		return ierr.NewError("wechatpay metadata is required").
			WithHint("WeChat Pay connection requires encrypted_secret_data.wechatpay").
			Mark(ierr.ErrValidation)
	}
	if w.AppID == "" {
		return ierr.NewError("appid is required").
			WithHint("WeChat Pay appid (AppID) is required").
			Mark(ierr.ErrValidation)
	}
	if w.MerchantID == "" {
		return ierr.NewError("mchid is required").
			WithHint("WeChat Pay merchant id (mchid) is required").
			Mark(ierr.ErrValidation)
	}
	if w.MerchantCertificateSerialNumber == "" {
		return ierr.NewError("merchant_certificate_serial_number is required").
			WithHint("The merchant certificate serial number is required to sign APIv3 requests").
			Mark(ierr.ErrValidation)
	}
	if w.MerchantPrivateKey == "" {
		return ierr.NewError("merchant_private_key is required").
			WithHint("The merchant private key (PEM) is required to sign APIv3 requests").
			Mark(ierr.ErrValidation)
	}
	if w.APIv3Key == "" {
		return ierr.NewError("api_v3_key is required").
			WithHint("The APIv3 key is required to verify and decrypt signed callbacks").
			Mark(ierr.ErrValidation)
	}
	if w.PlatformPublicKey == "" {
		return ierr.NewError("platform_public_key is required").
			WithHint("The WeChat Pay platform public key is required to verify signed responses and callbacks").
			Mark(ierr.ErrValidation)
	}
	if w.PlatformPublicKeyID == "" {
		return ierr.NewError("platform_public_key_id is required").
			WithHint("The WeChat Pay platform public key id is required to verify signed responses and callbacks").
			Mark(ierr.ErrValidation)
	}
	if w.NotifyURL == "" {
		return ierr.NewError("notify_url is required").
			WithHint("Provide the public https base URL of the WeChat Pay webhook route").
			Mark(ierr.ErrValidation)
	}
	return ValidateWechatPayNotifyURL(w.NotifyURL)
}

// NotifyURLFor builds the tenant/environment-scoped callback URL for payments
// made with this connection. The webhook route is
// /v1/webhooks/wechatpay/{tenant_id}/{environment_id}, so NotifyURL is the
// public base of that route and the routing segments are appended here rather
// than being supplied per request. Returns "" for a nil receiver or when the
// base URL or either id is empty.
func (w *WechatPayConnectionMetadata) NotifyURLFor(tenantID, environmentID string) string {
	if w == nil || w.NotifyURL == "" || tenantID == "" || environmentID == "" {
		return ""
	}
	return strings.TrimRight(w.NotifyURL, "/") + "/" + tenantID + "/" + environmentID
}

// ValidateWechatPayNotifyURL checks a notify URL is a public https URL that
// WeChat's servers can reach. Query, fragment, and userinfo components are
// rejected because the URL is used verbatim as the provider's notify_url, and
// any of them would change which request WeChat sends where. Loopback, private,
// link-local, and mDNS hosts are rejected because callbacks originate from the
// public internet. The path must be present: it is the prefix the tenant and
// environment routing segments are appended to.
func ValidateWechatPayNotifyURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ierr.NewError("notify_url is required").
			WithHint("Provide the public https base URL of the WeChat Pay webhook route").
			Mark(ierr.ErrValidation)
	}
	// The value is stored and passed to the provider verbatim, so surrounding or
	// embedded whitespace would corrupt the callback URL even though it parses.
	if strings.ContainsAny(raw, " \t\n\r\v\f") {
		return ierr.NewError("notify_url must not contain whitespace").
			WithHint("Remove any spaces or line breaks from notify_url").
			Mark(ierr.ErrValidation)
	}

	u, err := url.Parse(trimmed)
	if err != nil || !u.IsAbs() || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" {
		return ierr.NewError("notify_url must be a valid https URL").
			WithHint("WeChat Pay requires a public https notify_url, e.g. https://api.example.com/v1/webhooks/wechatpay").
			Mark(ierr.ErrValidation)
	}
	if u.User != nil {
		return ierr.NewError("notify_url must not include credentials").
			WithHint("Remove the userinfo component from notify_url").
			Mark(ierr.ErrValidation)
	}
	if u.RawQuery != "" {
		return ierr.NewError("notify_url must not include a query").
			WithHint("Remove the query component from notify_url; WeChat appends its own parameters").
			Mark(ierr.ErrValidation)
	}
	if u.Fragment != "" {
		return ierr.NewError("notify_url must not include a fragment").
			WithHint("Remove the fragment from notify_url").
			Mark(ierr.ErrValidation)
	}
	if u.Path == "" || u.Path == "/" {
		return ierr.NewError("notify_url must include the webhook route path").
			WithHint("Set notify_url to the public base of /v1/webhooks/wechatpay, e.g. https://api.example.com/v1/webhooks/wechatpay").
			Mark(ierr.ErrValidation)
	}

	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return ierr.NewError("notify_url host is not publicly reachable").
			WithHint("notify_url must be a public host; WeChat delivers callbacks from the internet").
			WithReportableDetails(map[string]any{"host": host}).
			Mark(ierr.ErrValidation)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsUnspecified() || ip.IsMulticast() {
			return ierr.NewError("notify_url host is not publicly reachable").
				WithHint("notify_url must be a public host; WeChat delivers callbacks from the internet").
				WithReportableDetails(map[string]any{"host": host}).
				Mark(ierr.ErrValidation)
		}
	}

	return nil
}
