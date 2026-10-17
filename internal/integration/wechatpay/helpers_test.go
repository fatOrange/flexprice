package wechatpay

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flexprice/flexprice/internal/config"
	"github.com/flexprice/flexprice/internal/domain/connection"
	"github.com/flexprice/flexprice/internal/logger"
	"github.com/flexprice/flexprice/internal/types"
	"github.com/stretchr/testify/require"
	"github.com/wechatpay-apiv3/wechatpay-go/utils"
)

// testMerchant* mirror the values in the stored connection fixture, so the
// mock can assert them on every signed request.
const (
	testAppID         = "wx-test-appid"
	testMerchantID    = "1900000001"
	testCertSerial    = "5157F09EFDC096DE5E5A8FCE1A8EA9A1D3A2A6C8"
	testPlatformKeyID = "PUB_KEY_ID_0000000000000000000000000000000001"
	testNotifyURLBase = "https://billing.example.com/v1/webhooks/wechatpay"
)

// testAPIv3Key is a valid 32-byte APIv3 key used to encrypt notification
// resources and decrypt them back in tests.
const testAPIv3Key = "0123456789abcdef0123456789abcdef"

// testKeys holds the key material one simulated merchant round trip needs: the
// merchant keypair the gateway signs requests with, and the platform keypair
// the mock provider signs responses and callbacks with.
type testKeys struct {
	merchantPrivate *rsa.PrivateKey
	platformPrivate *rsa.PrivateKey
}

func generateTestKeys(t *testing.T) *testKeys {
	t.Helper()
	merchant, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	platform, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return &testKeys{
		merchantPrivate: merchant,
		platformPrivate: platform,
	}
}

func (k *testKeys) merchantPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k.merchantPrivate)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// merchantPrivateKeyPKCS1PEM is the openssl genrsa form of the same key: the
// gateway must accept it because merchants paste either form.
func (k *testKeys) merchantPrivateKeyPKCS1PEM(t *testing.T) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k.merchantPrivate)}))
}

func (k *testKeys) platformPublicKeyPEM(t *testing.T) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&k.platformPrivate.PublicKey)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// testEncryptionService is a reversible stand-in for the AES service. The
// loader under test only needs the stored secret fields to be ciphertext that
// round-trips through Decrypt; the real AES implementation is outside this
// package.
type testEncryptionService struct{}

func (testEncryptionService) Encrypt(plaintext string) (string, error) {
	return "enc:" + plaintext, nil
}

func (testEncryptionService) Decrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	if !strings.HasPrefix(ciphertext, "enc:") {
		return "", errors.New("not ciphertext")
	}
	return strings.TrimPrefix(ciphertext, "enc:"), nil
}

func (testEncryptionService) Hash(value string) string { return value }

// fakeConnectionRepo serves one canned connection to GetByProvider; every
// other repository method is out of scope for these tests.
type fakeConnectionRepo struct {
	conn *connection.Connection
	err  error
}

func (f *fakeConnectionRepo) GetByProvider(ctx context.Context, provider types.SecretProvider) (*connection.Connection, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.conn, nil
}

func (f *fakeConnectionRepo) Create(ctx context.Context, conn *connection.Connection) error {
	return errors.New("not implemented")
}

func (f *fakeConnectionRepo) Get(ctx context.Context, id string) (*connection.Connection, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeConnectionRepo) ListPublishedByProvider(ctx context.Context, provider types.SecretProvider) ([]*connection.Connection, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeConnectionRepo) ListAllPublished(ctx context.Context) ([]*connection.Connection, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeConnectionRepo) List(ctx context.Context, filter *types.ConnectionFilter) ([]*connection.Connection, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeConnectionRepo) Count(ctx context.Context, filter *types.ConnectionFilter) (int, error) {
	return 0, errors.New("not implemented")
}

func (f *fakeConnectionRepo) Update(ctx context.Context, conn *connection.Connection) error {
	return errors.New("not implemented")
}

func (f *fakeConnectionRepo) Delete(ctx context.Context, conn *connection.Connection) error {
	return errors.New("not implemented")
}

// rewriteTransport reroutes the SDK's official API host to the loopback mock,
// preserving method, path, query, headers, and body. No base URL is overridden
// in production code; only the transport is swapped for the test.
type rewriteTransport struct {
	target *url.URL
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// recordedRequest captures what the SDK sent, so tests can assert on the wire
// contract (canonical fields, body).
type recordedRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
}

type mockResponse struct {
	Status int
	Body   string
	// NoSign omits the platform signature headers entirely, simulating a
	// response that cannot be authenticated.
	NoSign bool
	// BadSignature signs the body with the wrong key, simulating a forged
	// platform signature.
	BadSignature bool
}

// signedMockGateway is a loopback WeChat Pay APIv3 stand-in. It verifies the
// merchant's request signature with real RSA crypto and signs its responses
// with the platform key, exactly as the official SDK client expects.
type signedMockGateway struct {
	server *httptest.Server
	keys   *testKeys

	mu       sync.Mutex
	routes   map[string]func(*recordedRequest) mockResponse
	requests []recordedRequest
}

func newSignedMockGateway(t *testing.T, keys *testKeys) *signedMockGateway {
	t.Helper()
	gw := &signedMockGateway{keys: keys, routes: map[string]func(*recordedRequest) mockResponse{}}
	gw.server = httptest.NewServer(gw)
	t.Cleanup(gw.server.Close)
	return gw
}

func (m *signedMockGateway) handle(method, path string, handler func(*recordedRequest) mockResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes[method+" "+path] = handler
}

func (m *signedMockGateway) recorded() []recordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recordedRequest(nil), m.requests...)
}

var authorizationPattern = regexp.MustCompile(
	`^WECHATPAY2-SHA256-RSA2048 mchid="([^"]+)",nonce_str="([^"]+)",timestamp="([^"]+)",serial_no="([^"]+)",signature="([^"]+)"$`)

func (m *signedMockGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	recorded := recordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.Query(),
		Body:   body,
	}

	// Every request must carry a valid signature over the exact canonical
	// string the official SDK produces.
	if err := m.verifyAuthorization(r, body); err != nil {
		m.mu.Lock()
		m.requests = append(m.requests, recorded)
		m.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"SIGN_ERROR","message":"` + err.Error() + `"}`))
		return
	}
	// The SDK advertises which platform key serial it can verify.
	if serial := r.Header.Get("Wechatpay-Serial"); serial != testPlatformKeyID {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"SIGN_ERROR","message":"unexpected accept serial"}`))
		return
	}

	m.mu.Lock()
	m.requests = append(m.requests, recorded)
	handler, ok := m.routes[r.Method+" "+r.URL.Path]
	m.mu.Unlock()

	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"RESOURCE_NOT_EXISTS","message":"no mock route"}`))
		return
	}

	response := handler(&recorded)
	switch {
	case response.NoSign:
		w.WriteHeader(response.Status)
		_, _ = w.Write([]byte(response.Body))
	case response.BadSignature:
		// The merchant key is a real key but not the platform key the client
		// verifies with, so this signature must fail verification.
		m.writeSigned(w, response.Status, []byte(response.Body), m.keys.merchantPrivate)
	default:
		m.writeSigned(w, response.Status, []byte(response.Body), m.keys.platformPrivate)
	}
}

func (m *signedMockGateway) verifyAuthorization(r *http.Request, body []byte) error {
	parts := authorizationPattern.FindStringSubmatch(r.Header.Get("Authorization"))
	if parts == nil {
		return errors.New("authorization header does not match the SDK format")
	}
	mchID, nonce, timestamp, serial, signature := parts[1], parts[2], parts[3], parts[4], parts[5]
	if mchID != testMerchantID {
		return fmt.Errorf("unexpected mchid %s", mchID)
	}
	if serial != testCertSerial {
		return fmt.Errorf("unexpected serial %s", serial)
	}
	message := r.Method + "\n" + r.URL.RequestURI() + "\n" + timestamp + "\n" + nonce + "\n" + string(body) + "\n"
	signatureBytes, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return errors.New("signature is not base64")
	}
	hashed := sha256.Sum256([]byte(message))
	if err := rsa.VerifyPKCS1v15(&m.keys.merchantPrivate.PublicKey, crypto.SHA256, hashed[:], signatureBytes); err != nil {
		return errors.New("request signature does not verify")
	}
	return nil
}

// writeSigned signs a response body the way the WeChat Pay platform does:
// RSA-SHA256 over timestamp\nnonce\nbody\n, with the platform key id as serial.
// signWith lets tests deliberately sign with the wrong key.
func (m *signedMockGateway) writeSigned(w http.ResponseWriter, status int, body []byte, signWith *rsa.PrivateKey) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "mockresponsenonce"
	signature, err := utils.SignSHA256WithRSA(timestamp+"\n"+nonce+"\n"+string(body)+"\n", signWith)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Wechatpay-Timestamp", timestamp)
	w.Header().Set("Wechatpay-Nonce", nonce)
	w.Header().Set("Wechatpay-Serial", testPlatformKeyID)
	w.Header().Set("Wechatpay-Signature", signature)
	w.Header().Set("Request-Id", "mock-request-id")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// client builds the real Gateway against the loopback mock.
func (m *signedMockGateway) client(t *testing.T, repo connection.Repository) Gateway {
	t.Helper()
	log, err := logger.NewLogger(&config.Configuration{})
	require.NoError(t, err)
	target, err := url.Parse(m.server.URL)
	require.NoError(t, err)
	return NewClientWithHTTPClient(repo, testEncryptionService{}, log, &http.Client{
		Transport: &rewriteTransport{target: target},
		Timeout:   10 * time.Second,
	})
}

// connection is the stored WeChat Pay connection the gateway resolves: secrets
// are ciphertext, the platform public key and ids are plaintext.
func (m *signedMockGateway) connection(t *testing.T, keys *testKeys) *connection.Connection {
	t.Helper()
	return &connection.Connection{
		ID:           "conn_wechatpay_test",
		ProviderType: types.SecretProviderWechatPay,
		EncryptedSecretData: types.ConnectionMetadata{
			WechatPay: &types.WechatPayConnectionMetadata{
				AppID:                           testAppID,
				MerchantID:                      testMerchantID,
				MerchantCertificateSerialNumber: testCertSerial,
				MerchantPrivateKey:              "enc:" + keys.merchantPrivateKeyPEM(t),
				APIv3Key:                        "enc:" + testAPIv3Key,
				PlatformPublicKey:               keys.platformPublicKeyPEM(t),
				PlatformPublicKeyID:             testPlatformKeyID,
				NotifyURL:                       testNotifyURLBase,
			},
		},
	}
}

// testContext carries the tenant and environment the gateway resolves config
// routing from.
func testContext() context.Context {
	ctx := context.WithValue(context.Background(), types.CtxTenantID, "tenant_test")
	return context.WithValue(ctx, types.CtxEnvironmentID, "env_test")
}

// jsonResponse builds a platform-signed JSON response. WeChat Pay signs error
// responses exactly like successful ones, so signed is the realistic default;
// tests that need an unauthenticated response flip NoSign or BadSignature.
func jsonResponse(status int, payload any) mockResponse {
	body, _ := json.Marshal(payload)
	return mockResponse{Status: status, Body: string(body)}
}
