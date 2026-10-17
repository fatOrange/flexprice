package dto

import (
	"encoding/json"
	"testing"

	"github.com/flexprice/flexprice/internal/types"
	"github.com/stretchr/testify/require"
)

// These tests pin the encrypted_secret_data wire shapes the connection endpoints accept for
// WeChat Pay. Create takes create-style flat keys (mapped by ConvertFlatMetadataToStructured),
// while update accepts either the nested {"wechatpay": {...}} object or the same flat keys
// deferred to the service layer's provider-type conversion. The chosen shape decides which
// values the service merges, so a silent mis-route would turn a merchant key rotation into a
// no-op without any error — exactly the failure these assertions prevent. Values are
// deliberately fake and self-labelling.

func TestCreateConnectionRequest_WechatPayFlatSecretDataMapsEveryWireField(t *testing.T) {
	var req CreateConnectionRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"name": "WeChat Pay Primary",
		"provider_type": "wechatpay",
		"encrypted_secret_data": {
			"appid": "wx_test_appid_flexprice_0001",
			"mchid": "mchid_test_flexprice_0001",
			"merchant_certificate_serial_number": "TESTCERT0000000000000000000000000000000001",
			"merchant_private_key": "FAKE-TEST-ONLY-MERCHANT-PRIVATE-KEY-NOT-A-REAL-PEM",
			"api_v3_key": "FAKE-TEST-ONLY-APIv3-KEY-NOT-A-REAL-KEY",
			"platform_public_key": "FAKE-TEST-ONLY-PLATFORM-PUBLIC-KEY-NOT-A-REAL-PEM",
			"platform_public_key_id": "PUB_KEY_ID_TEST_0001",
			"notify_url": "https://api.flexprice.test/v1/webhooks/wechatpay"
		}
	}`), &req))

	require.Equal(t, types.SecretProviderWechatPay, req.ProviderType)
	require.NotNil(t, req.EncryptedSecretData.WechatPay)
	metadata := req.EncryptedSecretData.WechatPay
	require.Equal(t, "wx_test_appid_flexprice_0001", metadata.AppID)
	require.Equal(t, "mchid_test_flexprice_0001", metadata.MerchantID)
	require.Equal(t, "TESTCERT0000000000000000000000000000000001", metadata.MerchantCertificateSerialNumber)
	require.Equal(t, "FAKE-TEST-ONLY-MERCHANT-PRIVATE-KEY-NOT-A-REAL-PEM", metadata.MerchantPrivateKey)
	require.Equal(t, "FAKE-TEST-ONLY-APIv3-KEY-NOT-A-REAL-KEY", metadata.APIv3Key)
	require.Equal(t, "FAKE-TEST-ONLY-PLATFORM-PUBLIC-KEY-NOT-A-REAL-PEM", metadata.PlatformPublicKey)
	require.Equal(t, "PUB_KEY_ID_TEST_0001", metadata.PlatformPublicKeyID)
	require.Equal(t, "https://api.flexprice.test/v1/webhooks/wechatpay", metadata.NotifyURL)
}

func TestUpdateConnectionRequest_WechatPayNestedSecretDataStaysStructured(t *testing.T) {
	var req UpdateConnectionRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"encrypted_secret_data": {
			"wechatpay": {
				"merchant_private_key": "FAKE-TEST-ONLY-ROTATED-MERCHANT-PRIVATE-KEY-NOT-A-REAL-PEM",
				"api_v3_key": "FAKE-TEST-ONLY-ROTATED-APIv3-KEY-NOT-A-REAL-KEY"
			}
		}
	}`), &req))

	require.Nil(t, req.FlatEncryptedSecretData,
		"a nested wechatpay object must not fall through to the flat path, which maps no wechatpay keys and would no-op")
	require.NotNil(t, req.EncryptedSecretData)
	require.NotNil(t, req.EncryptedSecretData.WechatPay)
	require.Equal(t, "FAKE-TEST-ONLY-ROTATED-MERCHANT-PRIVATE-KEY-NOT-A-REAL-PEM",
		req.EncryptedSecretData.WechatPay.MerchantPrivateKey)
	require.Equal(t, "FAKE-TEST-ONLY-ROTATED-APIv3-KEY-NOT-A-REAL-KEY",
		req.EncryptedSecretData.WechatPay.APIv3Key)
}

func TestUpdateConnectionRequest_WechatPayFlatSecretDataDefersToProviderConversion(t *testing.T) {
	var req UpdateConnectionRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"encrypted_secret_data": {
			"merchant_private_key": "FAKE-TEST-ONLY-ROTATED-MERCHANT-PRIVATE-KEY-NOT-A-REAL-PEM",
			"api_v3_key": "FAKE-TEST-ONLY-ROTATED-APIv3-KEY-NOT-A-REAL-KEY"
		}
	}`), &req))

	require.Nil(t, req.EncryptedSecretData,
		"flat create-style keys carry no provider wrapper, so they must be deferred to the service layer")
	require.Equal(t, "FAKE-TEST-ONLY-ROTATED-MERCHANT-PRIVATE-KEY-NOT-A-REAL-PEM",
		req.FlatEncryptedSecretData["merchant_private_key"])
	require.Equal(t, "FAKE-TEST-ONLY-ROTATED-APIv3-KEY-NOT-A-REAL-KEY",
		req.FlatEncryptedSecretData["api_v3_key"])
}
