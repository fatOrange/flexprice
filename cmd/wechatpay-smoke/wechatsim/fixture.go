// Package wechatsim implements an isolated, clearly-fake simulation of the
// WeChat Pay APIv3 gateway (RFC: signed requests, signed responses, AES-GCM
// encrypted notifications). It exists only for the local wechatpay-smoke
// harness and the wechatpay tests outside internal/integration/wechatpay:
// no production code path may import or fall back to it, and every credential
// it mints is generated in memory on each run, prefixed FARB/FAKE, and never
// reaches a real WeChat endpoint.
package wechatsim

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// Fixture values are deliberately non-secret and self-describing. None of them
// is usable outside this simulated environment.
const (
	// FakeAppID models the WeChat app id bound to the connection.
	FakeAppID = "wxfakesim0000000001"
	// FakeMerchantID models the merchant number bound to the connection.
	FakeMerchantID = "1900000001"
	// FakeMerchantSerialNo models the merchant certificate serial number.
	FakeMerchantSerialNo = "FAKEMERCHANTSERIAL0000000001"
	// FakePlatformKeyID models WeChat's platform public key id (Wechatpay-Serial).
	FakePlatformKeyID = "PUB_KEY_ID_FAKE000000000000001"
	// FakeAPIv3Key is exactly 32 bytes, as the APIv3 AES-256-GCM key requires.
	FakeAPIv3Key = "0123456789abcdef0123456789abcdef"
)

// Fixture holds one fake merchant/platform key pair and the fake APIv3 key for
// a single smoke run. Two fixtures exist per run where an attacker identity is
// needed (foreign signature scenario).
type Fixture struct {
	AppID               string
	MerchantID          string
	MerchantSerialNo    string
	PlatformPublicKeyID string
	APIv3Key            []byte
	MerchantPrivateKey  *rsa.PrivateKey
	PlatformPrivateKey  *rsa.PrivateKey
}

// NewFixture generates a fresh in-memory fake identity. RSA key generation is
// the only slow step (2 x 2048 bit).
func NewFixture() (*Fixture, error) {
	merchantKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate fake merchant key: %w", err)
	}
	platformKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate fake platform key: %w", err)
	}
	return &Fixture{
		AppID:               FakeAppID,
		MerchantID:          FakeMerchantID,
		MerchantSerialNo:    FakeMerchantSerialNo,
		PlatformPublicKeyID: FakePlatformKeyID,
		APIv3Key:            []byte(FakeAPIv3Key),
		MerchantPrivateKey:  merchantKey,
		PlatformPrivateKey:  platformKey,
	}, nil
}

// MerchantPrivateKeyPEM renders the fake merchant private key in PKCS8 PEM, the
// format the production client parses from connection metadata.
func (f *Fixture) MerchantPrivateKeyPEM() string {
	der, err := x509.MarshalPKCS8PrivateKey(f.MerchantPrivateKey)
	if err != nil {
		panic(fmt.Sprintf("marshal fake merchant key: %v", err))
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// PlatformPublicKeyPEM renders the fake platform public key in PKIX PEM, the
// format stored in connection metadata for response/notification verification.
func (f *Fixture) PlatformPublicKeyPEM() string {
	der, err := x509.MarshalPKIXPublicKey(&f.PlatformPrivateKey.PublicKey)
	if err != nil {
		panic(fmt.Sprintf("marshal fake platform public key: %v", err))
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// MerchantPublicKey returns the public half of the fake merchant key, used by
// the simulator to verify APIv3 request signatures.
func (f *Fixture) MerchantPublicKey() *rsa.PublicKey {
	return &f.MerchantPrivateKey.PublicKey
}

// ParsePrivateKeyPEM parses a PKCS8 or PKCS1 PEM private key (test-only helper).
func ParsePrivateKeyPEM(pemData string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PEM block is not an RSA private key")
		}
		return rsaKey, nil
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// ParsePublicKeyPEM parses a PKIX PEM public key (test-only helper).
func ParsePublicKeyPEM(pemData string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("PEM block is not an RSA public key")
	}
	return rsaPub, nil
}
