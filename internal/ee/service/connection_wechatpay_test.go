package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/flexprice/flexprice/internal/api/dto"
	"github.com/flexprice/flexprice/internal/config"
	ierr "github.com/flexprice/flexprice/internal/errors"
	"github.com/flexprice/flexprice/internal/logger"
	"github.com/flexprice/flexprice/internal/security"
	"github.com/flexprice/flexprice/internal/testutil"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/stretchr/testify/require"
)

// Connection-level WeChat Pay tests. They exercise the CreateConnection /
// UpdateConnection / read-response seams of the connection service against the
// in-memory connection store, proving what actually lands in storage (AES-GCM
// ciphertext for the two secrets, plaintext for public configuration), that
// partial updates never reset or double-encrypt stored secrets, that a
// wrong-provider credential update is rejected before it can touch the row, and
// that no connection response carries secret material.
//
// The credentials below are deliberately fake and self-labelling. WeChat Pay
// connection validation only checks field presence (never key format), so these
// values are sufficient, and nothing here can be mistaken for a real merchant
// key, certificate or APIv3 key.
const (
	wechatPayTestAppID               = "wx_test_appid_flexprice_0001"
	wechatPayTestMerchantID          = "mchid_test_flexprice_0001"
	wechatPayTestCertSerial          = "TESTCERT0000000000000000000000000000000001"
	wechatPayTestPrivateKey          = "FAKE-TEST-ONLY-MERCHANT-PRIVATE-KEY-NOT-A-REAL-PEM"
	wechatPayTestAPIv3Key            = "FAKE-TEST-ONLY-APIv3-KEY-NOT-A-REAL-KEY"
	wechatPayTestPlatformPublicKey   = "FAKE-TEST-ONLY-PLATFORM-PUBLIC-KEY-NOT-A-REAL-PEM"
	wechatPayTestPlatformPublicKeyID = "PUB_KEY_ID_TEST_0001"
	wechatPayTestNotifyURL           = "https://api.flexprice.test/v1/webhooks/wechatpay"
	wechatPayTestEncryptionKey       = "test-encryption-key-for-unit-tests-only"
)

// newWechatPayConnectionServiceForTest wires a connectionService with the same
// in-memory repository, config and encryption construction used by
// newConnectionServiceForTest (connection_test.go), and additionally returns the
// encryption service so tests can decrypt stored rows — the exact operation the
// WeChat Pay gateway performs at runtime with the configured master key.
func newWechatPayConnectionServiceForTest(t *testing.T) (ConnectionService, *testutil.InMemoryConnectionStore, security.EncryptionService) {
	t.Helper()

	log := logger.NewNoopLogger()
	cfg := &config.Configuration{
		Secrets: config.SecretsConfig{EncryptionKey: wechatPayTestEncryptionKey},
	}
	encSvc, err := security.NewEncryptionService(cfg, log)
	require.NoError(t, err)

	connRepo := testutil.NewInMemoryConnectionStore()
	svc := NewConnectionService(ServiceParams{
		Logger:         log,
		Config:         cfg,
		ConnectionRepo: connRepo,
	}, encSvc)

	return svc, connRepo, encSvc
}

// newRuntimeWechatPayEncryptionService builds a second encryption service from the
// same master key, emulating the gateway process decrypting a stored row long after
// the API process encrypted it: stored ciphertext must stand on its own and not
// depend on any in-memory state of the service instance that wrote it.
func newRuntimeWechatPayEncryptionService(t *testing.T) security.EncryptionService {
	t.Helper()
	encSvc, err := security.NewEncryptionService(&config.Configuration{
		Secrets: config.SecretsConfig{EncryptionKey: wechatPayTestEncryptionKey},
	}, logger.NewNoopLogger())
	require.NoError(t, err)
	return encSvc
}

func wechatPayTestMetadata() types.ConnectionMetadata {
	return types.ConnectionMetadata{
		WechatPay: &types.WechatPayConnectionMetadata{
			AppID:                           wechatPayTestAppID,
			MerchantID:                      wechatPayTestMerchantID,
			MerchantCertificateSerialNumber: wechatPayTestCertSerial,
			MerchantPrivateKey:              wechatPayTestPrivateKey,
			APIv3Key:                        wechatPayTestAPIv3Key,
			PlatformPublicKey:               wechatPayTestPlatformPublicKey,
			PlatformPublicKeyID:             wechatPayTestPlatformPublicKeyID,
			NotifyURL:                       wechatPayTestNotifyURL,
		},
	}
}

// wechatPayTestMetadataPtr is wechatPayTestMetadata for call sites whose DTO field is a
// *types.ConnectionMetadata (UpdateConnectionRequest), rather than the value field
// CreateConnectionRequest uses.
func wechatPayTestMetadataPtr() *types.ConnectionMetadata {
	metadata := wechatPayTestMetadata()
	return &metadata
}

// seedWechatPayConnection creates the single WeChat Pay connection each test starts
// from (the provider is covered by the one-published-connection-per-environment
// rule). Every test builds its own service/repository pair, so seeds never collide.
func seedWechatPayConnection(t *testing.T, svc ConnectionService, ctx context.Context) *dto.ConnectionResponse {
	t.Helper()
	resp, err := svc.CreateConnection(ctx, dto.CreateConnectionRequest{
		Name:                "WeChat Pay Primary",
		ProviderType:        types.SecretProviderWechatPay,
		EncryptedSecretData: wechatPayTestMetadata(),
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	return resp
}

func storedWechatPayMetadata(t *testing.T, repo *testutil.InMemoryConnectionStore, ctx context.Context, id string) *types.WechatPayConnectionMetadata {
	t.Helper()
	conn, err := repo.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, conn.EncryptedSecretData.WechatPay, "stored connection must carry wechatpay metadata")
	return conn.EncryptedSecretData.WechatPay
}

func mustDecryptWechatPaySecret(t *testing.T, encSvc security.EncryptionService, stored string) string {
	t.Helper()
	plaintext, err := encSvc.Decrypt(stored)
	require.NoError(t, err, "stored WeChat Pay secret must be decryptable ciphertext")
	return plaintext
}

// TestCreateConnection_WechatPay_EncryptsSecretFieldsAtRest proves the merchant
// private key and APIv3 key are never persisted in the clear, while the public
// merchant configuration stays readable exactly as stored: the gateway reads
// appid/mchid/serial/platform key/notify_url directly, so over-encrypting them
// would break runtime consumers just as leaving the private key readable would
// break security.
func TestCreateConnection_WechatPay_EncryptsSecretFieldsAtRest(t *testing.T) {
	svc, repo, encSvc := newWechatPayConnectionServiceForTest(t)
	ctx := testutil.SetupContext()

	created := seedWechatPayConnection(t, svc, ctx)
	stored := storedWechatPayMetadata(t, repo, ctx, created.ID)

	require.Equal(t, wechatPayTestAppID, stored.AppID)
	require.Equal(t, wechatPayTestMerchantID, stored.MerchantID)
	require.Equal(t, wechatPayTestCertSerial, stored.MerchantCertificateSerialNumber)
	require.Equal(t, wechatPayTestPlatformPublicKey, stored.PlatformPublicKey)
	require.Equal(t, wechatPayTestPlatformPublicKeyID, stored.PlatformPublicKeyID)
	require.Equal(t, wechatPayTestNotifyURL, stored.NotifyURL)

	require.NotEqual(t, wechatPayTestPrivateKey, stored.MerchantPrivateKey,
		"the merchant private key must not be stored in the clear")
	require.NotContains(t, stored.MerchantPrivateKey, wechatPayTestPrivateKey)
	require.Equal(t, wechatPayTestPrivateKey, mustDecryptWechatPaySecret(t, encSvc, stored.MerchantPrivateKey))

	require.NotEqual(t, wechatPayTestAPIv3Key, stored.APIv3Key,
		"the APIv3 key must not be stored in the clear")
	require.NotContains(t, stored.APIv3Key, wechatPayTestAPIv3Key)
	require.Equal(t, wechatPayTestAPIv3Key, mustDecryptWechatPaySecret(t, encSvc, stored.APIv3Key))

	require.NotEqual(t, stored.MerchantPrivateKey, stored.APIv3Key,
		"two distinct secrets must not share a stored representation")
}

// TestCreateConnection_WechatPay_RejectsNotifyURLsWeChatCouldNotReach pins create-time
// validation of the URL WeChat's servers dial: a query or fragment changes which request
// arrives where, and loopback hosts are unreachable from the public internet. Each
// rejected create must leave no connection row behind.
func TestCreateConnection_WechatPay_RejectsNotifyURLsWeChatCouldNotReach(t *testing.T) {
	cases := []struct {
		name      string
		notifyURL string
	}{
		{"query_component", wechatPayTestNotifyURL + "?tenant=1"},
		{"fragment_component", wechatPayTestNotifyURL + "#fragment"},
		{"localhost_host", "https://localhost/v1/webhooks/wechatpay"},
		{"loopback_ip_host", "https://127.0.0.1/v1/webhooks/wechatpay"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo, _ := newWechatPayConnectionServiceForTest(t)
			ctx := testutil.SetupContext()

			metadata := wechatPayTestMetadata()
			metadata.WechatPay.NotifyURL = tc.notifyURL

			resp, err := svc.CreateConnection(ctx, dto.CreateConnectionRequest{
				Name:                "WeChat Pay Primary",
				ProviderType:        types.SecretProviderWechatPay,
				EncryptedSecretData: metadata,
			})
			require.Error(t, err)
			require.True(t, ierr.IsValidation(err), "expected validation error, got: %v", err)
			require.Nil(t, resp)

			persisted, err := repo.List(ctx, &types.ConnectionFilter{ProviderType: types.SecretProviderWechatPay})
			require.NoError(t, err)
			require.Empty(t, persisted, "a rejected notify_url must not leave a connection row behind")
		})
	}
}

// TestUpdateConnection_WechatPay_UnrelatedPartialUpdatePreservesStoredSecrets is the
// reset/re-encryption guard: an update that only changes non-secret fields must leave the
// stored ciphertext byte-for-byte untouched (re-encrypting the same plaintext would produce
// different ciphertext because AES-GCM uses a random nonce, so exact equality catches both
// double-encryption and a plaintext rewrite), and both secrets must stay decryptable.
func TestUpdateConnection_WechatPay_UnrelatedPartialUpdatePreservesStoredSecrets(t *testing.T) {
	svc, repo, encSvc := newWechatPayConnectionServiceForTest(t)
	ctx := testutil.SetupContext()

	created := seedWechatPayConnection(t, svc, ctx)
	before := storedWechatPayMetadata(t, repo, ctx, created.ID)
	// Capture strings, not the returned struct: the in-memory store hands out metadata
	// whose nested structs alias the stored row.
	beforePrivateKey, beforeAPIv3Key := before.MerchantPrivateKey, before.APIv3Key

	rotatedAppID := "wx_test_appid_flexprice_0002"
	updatedNotifyURL := wechatPayTestNotifyURL + "-v2"

	updated, err := svc.UpdateConnection(ctx, created.ID, dto.UpdateConnectionRequest{
		Name: "WeChat Pay Primary Renamed",
		EncryptedSecretData: &types.ConnectionMetadata{
			WechatPay: &types.WechatPayConnectionMetadata{
				AppID:     rotatedAppID,
				NotifyURL: updatedNotifyURL,
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, "WeChat Pay Primary Renamed", updated.Name)

	after := storedWechatPayMetadata(t, repo, ctx, created.ID)
	require.Equal(t, beforePrivateKey, after.MerchantPrivateKey,
		"an unrelated update must not re-encrypt or reset the merchant private key")
	require.Equal(t, beforeAPIv3Key, after.APIv3Key,
		"an unrelated update must not re-encrypt or reset the APIv3 key")
	require.Equal(t, wechatPayTestPrivateKey, mustDecryptWechatPaySecret(t, encSvc, after.MerchantPrivateKey))
	require.Equal(t, wechatPayTestAPIv3Key, mustDecryptWechatPaySecret(t, encSvc, after.APIv3Key))

	require.Equal(t, rotatedAppID, after.AppID)
	require.Equal(t, updatedNotifyURL, after.NotifyURL)
	require.Equal(t, wechatPayTestMerchantID, after.MerchantID,
		"fields omitted from the update must keep their stored values")
	require.Equal(t, wechatPayTestCertSerial, after.MerchantCertificateSerialNumber)
}

// TestUpdateConnection_WechatPay_FlatWireBodyRotatesSecretsAndStaysDecryptable covers the
// merchant-key rotation path exactly as an HTTP client submits it: create-style flat keys
// under encrypted_secret_data, converted using the connection's provider type. Rotated keys
// must replace the stored ciphertext and be decryptable by a freshly constructed encryption
// service (what the gateway process does at runtime), while fields the client did not send
// are preserved.
func TestUpdateConnection_WechatPay_FlatWireBodyRotatesSecretsAndStaysDecryptable(t *testing.T) {
	svc, repo, _ := newWechatPayConnectionServiceForTest(t)
	ctx := testutil.SetupContext()

	created := seedWechatPayConnection(t, svc, ctx)
	before := storedWechatPayMetadata(t, repo, ctx, created.ID)
	// Capture strings, not the returned struct: the in-memory store hands out metadata
	// whose nested structs alias the stored row, so the pointer would show post-update values.
	beforePrivateKey, beforeAPIv3Key := before.MerchantPrivateKey, before.APIv3Key

	newPrivateKey := "FAKE-TEST-ONLY-ROTATED-MERCHANT-PRIVATE-KEY-NOT-A-REAL-PEM"
	newAPIv3Key := "FAKE-TEST-ONLY-ROTATED-APIv3-KEY-NOT-A-REAL-KEY"

	body := fmt.Sprintf(
		`{"encrypted_secret_data":{"merchant_private_key":%q,"api_v3_key":%q}}`,
		newPrivateKey, newAPIv3Key,
	)
	var req dto.UpdateConnectionRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))

	updated, err := svc.UpdateConnection(ctx, created.ID, req)
	require.NoError(t, err)
	require.NotNil(t, updated)

	after := storedWechatPayMetadata(t, repo, ctx, created.ID)
	require.NotEqual(t, beforePrivateKey, after.MerchantPrivateKey,
		"rotation must replace the stored private key ciphertext")
	require.NotEqual(t, beforeAPIv3Key, after.APIv3Key,
		"rotation must replace the stored APIv3 key ciphertext")
	require.NotEqual(t, newPrivateKey, after.MerchantPrivateKey)
	require.NotEqual(t, newAPIv3Key, after.APIv3Key)

	runtimeEncSvc := newRuntimeWechatPayEncryptionService(t)
	require.Equal(t, newPrivateKey, mustDecryptWechatPaySecret(t, runtimeEncSvc, after.MerchantPrivateKey),
		"the rotated private key must be usable by a freshly constructed encryption service")
	require.Equal(t, newAPIv3Key, mustDecryptWechatPaySecret(t, runtimeEncSvc, after.APIv3Key),
		"the rotated APIv3 key must be usable by a freshly constructed encryption service")

	require.Equal(t, wechatPayTestAppID, after.AppID,
		"fields absent from the flat body must keep their stored values")
	require.Equal(t, wechatPayTestMerchantID, after.MerchantID)
	require.Equal(t, wechatPayTestNotifyURL, after.NotifyURL)
}

// TestUpdateConnection_WechatPay_WrongProviderSecretUpdateRejectedAndSecretsUntouched
// proves the provider guard fires before any secret is written or replaced: WeChat Pay
// credentials cannot be injected into another provider's connection and another provider's
// credentials cannot be injected into a WeChat Pay connection, so a mis-wired client cannot
// coax the service into encrypting credentials onto the wrong row.
func TestUpdateConnection_WechatPay_WrongProviderSecretUpdateRejectedAndSecretsUntouched(t *testing.T) {
	t.Run("wechatpay_secrets_on_stripe_connection", func(t *testing.T) {
		svc, repo, encSvc := newWechatPayConnectionServiceForTest(t)
		ctx := testutil.SetupContext()

		created, err := svc.CreateConnection(ctx, dto.CreateConnectionRequest{
			Name:         "Stripe Connection",
			ProviderType: types.SecretProviderStripe,
			EncryptedSecretData: types.ConnectionMetadata{
				Stripe: &types.StripeConnectionMetadata{
					PublishableKey: "pk_test_wrong_provider_0001",
					SecretKey:      "sk_test_wrong_provider_0001",
				},
			},
		})
		require.NoError(t, err)

		before, err := repo.Get(ctx, created.ID)
		require.NoError(t, err)
		stripeSecretBefore := before.EncryptedSecretData.Stripe.SecretKey

		resp, err := svc.UpdateConnection(ctx, created.ID, dto.UpdateConnectionRequest{
			EncryptedSecretData: wechatPayTestMetadataPtr(),
		})
		require.Error(t, err)
		require.True(t, ierr.IsValidation(err), "expected validation error, got: %v", err)
		require.Nil(t, resp)

		after, err := repo.Get(ctx, created.ID)
		require.NoError(t, err)
		require.Nil(t, after.EncryptedSecretData.WechatPay,
			"wechatpay secrets must not be written onto a stripe connection")
		require.Equal(t, stripeSecretBefore, after.EncryptedSecretData.Stripe.SecretKey,
			"the rejected update must not replace the stored stripe secret")
		require.Equal(t, "sk_test_wrong_provider_0001",
			mustDecryptWechatPaySecret(t, encSvc, after.EncryptedSecretData.Stripe.SecretKey))
	})

	t.Run("stripe_secrets_on_wechatpay_connection", func(t *testing.T) {
		svc, repo, encSvc := newWechatPayConnectionServiceForTest(t)
		ctx := testutil.SetupContext()

		created := seedWechatPayConnection(t, svc, ctx)
		before := storedWechatPayMetadata(t, repo, ctx, created.ID)
		beforePrivateKey, beforeAPIv3Key := before.MerchantPrivateKey, before.APIv3Key

		resp, err := svc.UpdateConnection(ctx, created.ID, dto.UpdateConnectionRequest{
			EncryptedSecretData: &types.ConnectionMetadata{
				Stripe: &types.StripeConnectionMetadata{WebhookSecret: "whsec_wrong_provider_0001"},
			},
		})
		require.Error(t, err)
		require.True(t, ierr.IsValidation(err), "expected validation error, got: %v", err)
		require.Nil(t, resp)

		storedConn, err := repo.Get(ctx, created.ID)
		require.NoError(t, err)
		require.Nil(t, storedConn.EncryptedSecretData.Stripe,
			"stripe secrets must not be written onto a wechatpay connection")
		require.Equal(t, beforePrivateKey, storedConn.EncryptedSecretData.WechatPay.MerchantPrivateKey)
		require.Equal(t, beforeAPIv3Key, storedConn.EncryptedSecretData.WechatPay.APIv3Key)
		require.Equal(t, wechatPayTestPrivateKey,
			mustDecryptWechatPaySecret(t, encSvc, storedConn.EncryptedSecretData.WechatPay.MerchantPrivateKey))
	})
}

// TestUpdateConnection_WechatPay_InvalidNotifyURLRejectedAndStoredBaseUnchanged proves the
// update path validates the incoming notify_url as plaintext before it can replace the
// stored callback base, so a connection can never be left pointing WeChat at a URL it
// cannot reach or that has a query/fragment attached.
func TestUpdateConnection_WechatPay_InvalidNotifyURLRejectedAndStoredBaseUnchanged(t *testing.T) {
	cases := []struct {
		name      string
		notifyURL string
	}{
		{"query_component", wechatPayTestNotifyURL + "?tenant=1"},
		{"fragment_component", wechatPayTestNotifyURL + "#fragment"},
		{"localhost_host", "https://localhost/v1/webhooks/wechatpay"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo, encSvc := newWechatPayConnectionServiceForTest(t)
			ctx := testutil.SetupContext()

			created := seedWechatPayConnection(t, svc, ctx)
			before := storedWechatPayMetadata(t, repo, ctx, created.ID)
			beforePrivateKey := before.MerchantPrivateKey

			resp, err := svc.UpdateConnection(ctx, created.ID, dto.UpdateConnectionRequest{
				EncryptedSecretData: &types.ConnectionMetadata{
					WechatPay: &types.WechatPayConnectionMetadata{NotifyURL: tc.notifyURL},
				},
			})
			require.Error(t, err)
			require.True(t, ierr.IsValidation(err), "expected validation error, got: %v", err)
			require.Nil(t, resp)

			after := storedWechatPayMetadata(t, repo, ctx, created.ID)
			require.Equal(t, wechatPayTestNotifyURL, after.NotifyURL,
				"a rejected notify_url must not replace the stored callback base")
			require.Equal(t, beforePrivateKey, after.MerchantPrivateKey)
			require.Equal(t, wechatPayTestPrivateKey, mustDecryptWechatPaySecret(t, encSvc, after.MerchantPrivateKey))
		})
	}
}

// TestConnection_WechatPay_ResponsesNeverExposeSecretMaterial guards the read surface an
// API client or dashboard sees: the create, get, list and update responses must not carry
// the plaintext private key or APIv3 key, their stored ciphertext, or even the wire keys of
// the two secret fields. Any of those would put signing/decryption material into logs,
// browser state or SDK models.
func TestConnection_WechatPay_ResponsesNeverExposeSecretMaterial(t *testing.T) {
	svc, repo, _ := newWechatPayConnectionServiceForTest(t)
	ctx := testutil.SetupContext()

	created := seedWechatPayConnection(t, svc, ctx)
	stored := storedWechatPayMetadata(t, repo, ctx, created.ID)

	forbidden := []string{
		wechatPayTestPrivateKey,
		wechatPayTestAPIv3Key,
		stored.MerchantPrivateKey,
		stored.APIv3Key,
		`"merchant_private_key"`,
		`"api_v3_key"`,
	}

	assertJSONLacksSecretMaterial(t, "create", created, forbidden...)

	got, err := svc.GetConnection(ctx, created.ID)
	require.NoError(t, err)
	assertJSONLacksSecretMaterial(t, "get", got, forbidden...)

	list, err := svc.GetConnections(ctx, types.NewNoLimitConnectionFilter())
	require.NoError(t, err)
	assertJSONLacksSecretMaterial(t, "list", list, forbidden...)

	rotatedPrivateKey := "FAKE-TEST-ONLY-ROTATED-MERCHANT-PRIVATE-KEY-NOT-A-REAL-PEM"
	rotatedAPIv3Key := "FAKE-TEST-ONLY-ROTATED-APIv3-KEY-NOT-A-REAL-KEY"
	updated, err := svc.UpdateConnection(ctx, created.ID, dto.UpdateConnectionRequest{
		EncryptedSecretData: &types.ConnectionMetadata{
			WechatPay: &types.WechatPayConnectionMetadata{
				MerchantPrivateKey: rotatedPrivateKey,
				APIv3Key:           rotatedAPIv3Key,
			},
		},
	})
	require.NoError(t, err)
	assertJSONLacksSecretMaterial(t, "update",
		updated, rotatedPrivateKey, rotatedAPIv3Key, `"merchant_private_key"`, `"api_v3_key"`)
}

func assertJSONLacksSecretMaterial(t *testing.T, surface string, payload any, forbidden ...string) {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	for _, value := range forbidden {
		require.NotContains(t, string(raw), value,
			"%s response JSON must not contain WeChat Pay secret material", surface)
	}
}

// TestUpdateConnection_WechatPay_CrossTenantCallNeverAdoptsCallerIdentity pins the tenant
// safety of the update path: the stored row keeps its owning tenant and environment even if
// an update call arrives under another tenant's context, and the connection stays invisible
// to that tenant's listing. The Ent repository scopes Get by tenant+environment, so
// production rejects such a call outright; the in-memory store is keyed by connection id
// only, so this test asserts the invariants that must hold under either behaviour: the row
// is never re-homed and its secrets are never replaced by a different tenant's call.
func TestUpdateConnection_WechatPay_CrossTenantCallNeverAdoptsCallerIdentity(t *testing.T) {
	svc, repo, _ := newWechatPayConnectionServiceForTest(t)
	ownerCtx := testutil.SetupContext()

	created := seedWechatPayConnection(t, svc, ownerCtx)
	before := storedWechatPayMetadata(t, repo, ownerCtx, created.ID)
	beforePrivateKey, beforeAPIv3Key := before.MerchantPrivateKey, before.APIv3Key

	ownerTenantID := types.GetTenantID(ownerCtx)
	ownerEnvironmentID := types.GetEnvironmentID(ownerCtx)

	otherCtx := types.SetTenantID(context.Background(), "tenant_wechatpay_other")
	otherCtx = types.SetEnvironmentID(otherCtx, ownerEnvironmentID)

	_, _ = svc.UpdateConnection(otherCtx, created.ID, dto.UpdateConnectionRequest{Name: "renamed by another tenant"})

	storedConn, err := repo.Get(ownerCtx, created.ID)
	require.NoError(t, err)
	require.Equal(t, ownerTenantID, storedConn.TenantID,
		"an update must never re-home a connection into the caller's tenant")
	require.Equal(t, ownerEnvironmentID, storedConn.EnvironmentID,
		"an update must never re-home a connection into the caller's environment")
	require.Equal(t, beforePrivateKey, storedConn.EncryptedSecretData.WechatPay.MerchantPrivateKey)
	require.Equal(t, beforeAPIv3Key, storedConn.EncryptedSecretData.WechatPay.APIv3Key)

	otherList, err := svc.GetConnections(otherCtx, types.NewNoLimitConnectionFilter())
	require.NoError(t, err)
	require.Empty(t, otherList.Connections,
		"the connection must stay invisible to another tenant's listing")

	ownerList, err := svc.GetConnections(ownerCtx, types.NewNoLimitConnectionFilter())
	require.NoError(t, err)
	require.Len(t, ownerList.Connections, 1)
}
