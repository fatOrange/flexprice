package wechatpay

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"

	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/wechatpay-apiv3/wechatpay-go/utils"
)

// apiV3KeyLength is the fixed length of a WeChat Pay APIv3 key, which is used
// as a raw AES-256-GCM key for notification resources.
const apiV3KeyLength = 32

// connectionConfig is the decrypted, per-request view of one WeChat Pay
// connection. It is rebuilt from the connection repository on every call, so
// rotating any credential takes effect on the next request; nothing is cached
// across tenants, environments, or requests.
type connectionConfig struct {
	appID               string
	merchantID          string
	merchantSerialNo    string
	merchantPrivateKey  *rsa.PrivateKey
	apiV3Key            string
	platformPublicKey   *rsa.PublicKey
	platformPublicKeyID string
	notifyURL           string
}

// loadConnectionConfig resolves the WeChat Pay connection for the request's
// tenant and environment and decrypts the two secret fields.
func (c *client) loadConnectionConfig(ctx context.Context) (*connectionConfig, error) {
	conn, err := c.connectionRepo.GetByProvider(ctx, types.SecretProviderWechatPay)
	if err != nil {
		return nil, ierr.WithError(err).
			WithHint("WeChat Pay connection not configured for this environment").
			Mark(ierr.ErrNotFound)
	}
	if conn == nil {
		return nil, ierr.NewError("wechatpay connection not found").
			WithHint("WeChat Pay connection not configured for this environment").
			Mark(ierr.ErrNotFound)
	}

	meta := conn.EncryptedSecretData.WechatPay
	if meta == nil {
		return nil, ierr.NewError("wechatpay connection metadata is missing").
			WithHint("Configure the WeChat Pay connection with appid, mchid, certificates, public key, and notify_url").
			WithReportableDetails(map[string]any{"connection_id": conn.ID}).
			Mark(ierr.ErrNotFound)
	}
	for _, required := range []struct {
		name  string
		value string
	}{
		{"appid", meta.AppID},
		{"mchid", meta.MerchantID},
		{"merchant_certificate_serial_number", meta.MerchantCertificateSerialNumber},
		{"platform_public_key_id", meta.PlatformPublicKeyID},
	} {
		if required.value == "" {
			return nil, ierr.NewError("wechatpay connection is incomplete").
				WithHintf("The %s field is missing on the WeChat Pay connection", required.name).
				WithReportableDetails(map[string]any{"connection_id": conn.ID, "field": required.name}).
				Mark(ierr.ErrValidation)
		}
	}

	privateKeyPEM, err := c.encryptionService.Decrypt(meta.MerchantPrivateKey)
	if err != nil {
		return nil, ierr.WithError(err).
			WithHint("The WeChat Pay merchant private key could not be decrypted").
			WithReportableDetails(map[string]any{"connection_id": conn.ID}).
			Mark(ierr.ErrInternal)
	}
	apiV3Key, err := c.encryptionService.Decrypt(meta.APIv3Key)
	if err != nil {
		return nil, ierr.WithError(err).
			WithHint("The WeChat Pay APIv3 key could not be decrypted").
			WithReportableDetails(map[string]any{"connection_id": conn.ID}).
			Mark(ierr.ErrInternal)
	}

	privateKey, err := loadMerchantPrivateKey(privateKeyPEM)
	if err != nil {
		return nil, ierr.WithError(err).
			WithHint("The WeChat Pay merchant private key is not a valid RSA private key in PEM form").
			Mark(ierr.ErrValidation)
	}
	platformPublicKey, err := utils.LoadPublicKey(meta.PlatformPublicKey)
	if err != nil {
		return nil, ierr.WithError(err).
			WithHint("The WeChat Pay platform public key is not a valid RSA public key in PEM form").
			Mark(ierr.ErrValidation)
	}
	if len(apiV3Key) != apiV3KeyLength {
		return nil, ierr.NewError("wechatpay api v3 key must be 32 bytes").
			WithHint("Copy the 32-character APIv3 key exactly as set in the WeChat merchant platform").
			Mark(ierr.ErrValidation)
	}

	notifyURL := meta.NotifyURLFor(types.GetTenantID(ctx), types.GetEnvironmentID(ctx))
	if notifyURL == "" {
		return nil, ierr.NewError("wechatpay notify url is not configured").
			WithHint("Set notify_url to the public base of /v1/webhooks/wechatpay on the connection").
			Mark(ierr.ErrValidation)
	}

	return &connectionConfig{
		appID:               meta.AppID,
		merchantID:          meta.MerchantID,
		merchantSerialNo:    meta.MerchantCertificateSerialNumber,
		merchantPrivateKey:  privateKey,
		apiV3Key:            apiV3Key,
		platformPublicKey:   platformPublicKey,
		platformPublicKeyID: meta.PlatformPublicKeyID,
		notifyURL:           notifyURL,
	}, nil
}

// loadMerchantPrivateKey accepts the PKCS#8 PEM that WeChat's key tooling
// exports and the PKCS#1 PEM openssl writes, so either form can be pasted into
// the connection.
func loadMerchantPrivateKey(privateKeyPEM string) (*rsa.PrivateKey, error) {
	if key, err := utils.LoadPrivateKey(privateKeyPEM); err == nil {
		return key, nil
	}
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, errors.New("private key is not PEM encoded")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}
