package wechatsim

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Signature message formats, byte-for-byte identical to the WeChat Pay APIv3
// specification and the official Go SDK (core/consts):
//
//	request:  METHOD\nCANONICAL_URL\nTIMESTAMP\nNONCE\nBODY\n
//	response: TIMESTAMP\nNONCE\nBODY\n
func RequestSignatureMessage(method, canonicalURL, timestamp, nonce, body string) string {
	return fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n", method, canonicalURL, timestamp, nonce, body)
}

// ResponseSignatureMessage is the message WeChat signs on every HTTP response.
func ResponseSignatureMessage(timestamp, nonce string, body []byte) string {
	return fmt.Sprintf("%s\n%s\n%s\n", timestamp, nonce, string(body))
}

// SignSHA256WithRSA mirrors the official SDK's signing primitive.
func SignSHA256WithRSA(message string, key *rsa.PrivateKey) (string, error) {
	hashed := sha256.Sum256([]byte(message))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// VerifySHA256WithRSA verifies a base64 PKCS1v15 SHA-256 signature.
func VerifySHA256WithRSA(message, signatureB64 string, pub *rsa.PublicKey) error {
	sig, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return fmt.Errorf("signature is not base64: %w", err)
	}
	hashed := sha256.Sum256([]byte(message))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, hashed[:], sig); err != nil {
		return fmt.Errorf("rsa verify failed: %w", err)
	}
	return nil
}

// AuthorizationParts is the parsed WECHATPAY2 Authorization header.
type AuthorizationParts struct {
	MchID     string
	Nonce     string
	Timestamp string
	SerialNo  string
	Signature string
}

// ParseAuthorizationHeader parses the APIv3 Authorization header the official
// SDK sends:
//
//	WECHATPAY2-SHA256-RSA2048 mchid="...",nonce_str="...",timestamp="...",serial_no="...",signature="..."
//
// The parameters are comma-separated after the scheme token; each value is
// quoted. Splitting on whitespace is not enough — the whole parameter list is
// a single whitespace-separated token — so the list is split on commas first.
func ParseAuthorizationHeader(header string) (*AuthorizationParts, error) {
	scheme, params, found := strings.Cut(header, " ")
	if !found || !strings.HasPrefix(scheme, "WECHATPAY2-") {
		return nil, fmt.Errorf("unrecognized authorization scheme %q", header)
	}
	parts := &AuthorizationParts{}
	for _, entry := range strings.Split(params, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"`)
		switch key {
		case "mchid":
			parts.MchID = value
		case "nonce_str":
			parts.Nonce = value
		case "timestamp":
			parts.Timestamp = value
		case "serial_no":
			parts.SerialNo = value
		case "signature":
			parts.Signature = value
		}
	}
	if parts.MchID == "" || parts.Nonce == "" || parts.Timestamp == "" || parts.SerialNo == "" || parts.Signature == "" {
		return nil, fmt.Errorf("incomplete authorization header %q", header)
	}
	if _, err := strconv.ParseInt(parts.Timestamp, 10, 64); err != nil {
		return nil, fmt.Errorf("non-numeric timestamp %q", parts.Timestamp)
	}
	return parts, nil
}

// SignedNotification builds a WeChat-style notification: AES-256-GCM encrypted
// resource plus platform-signed envelope headers. A test can pass a plaintext it
// crafted itself to simulate forged or inconsistent payloads.
func (f *Fixture) SignedNotification(eventType, originalType string, plaintext []byte) (http.Header, []byte, error) {
	nonce, associatedData, ciphertext, err := encryptResource(f.APIv3Key, plaintext)
	if err != nil {
		return nil, nil, err
	}
	envelope := map[string]any{
		"id":            fmt.Sprintf("FAKENOTIFY%06d", time.Now().UnixNano()%1_000_000),
		"create_time":   time.Now().Format("2006-01-02T15:04:05+08:00"),
		"event_type":    eventType,
		"resource_type": "encrypt-resource",
		"resource": map[string]string{
			"algorithm":       "AEAD_AES_256_GCM",
			"ciphertext":      ciphertext,
			"nonce":           nonce,
			"associated_data": associatedData,
			"original_type":   originalType,
		},
		"summary": "fake local simulation",
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, nil, err
	}
	headers, err := f.SignNotificationBody(body)
	if err != nil {
		return nil, nil, err
	}
	return headers, body, nil
}

// SignNotificationBody platform-signs an already-built notification body using
// the same header set and message format WeChat uses.
func (f *Fixture) SignNotificationBody(body []byte) (http.Header, error) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	signature, err := SignSHA256WithRSA(ResponseSignatureMessage(timestamp, nonce, body), f.PlatformPrivateKey)
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	headers.Set("Wechatpay-Timestamp", timestamp)
	headers.Set("Wechatpay-Nonce", nonce)
	headers.Set("Wechatpay-Signature", signature)
	headers.Set("Wechatpay-Serial", f.PlatformPublicKeyID)
	headers.Set("Content-Type", "application/json")
	return headers, nil
}

// encryptResource implements AES-256-GCM in WeChat's layout: a 12-byte nonce
// string, arbitrary associated data, and a base64 ciphertext carrying the GCM
// tag. The nonce and associated data travel in the envelope as raw strings and
// the official SDK passes their bytes straight to cipher.AEAD.Open (no base64
// decoding of the nonce), so the nonce string must itself be exactly
// cipher.NewGCM's standard nonce size — here 12 hex characters.
func encryptResource(key, plaintext []byte) (nonce, associatedData, ciphertext string, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", "", err
	}
	nonce, err = randomHex(gcm.NonceSize() / 2)
	if err != nil {
		return "", "", "", err
	}
	if len(nonce) != gcm.NonceSize() {
		return "", "", "", fmt.Errorf("fake resource nonce is %d bytes, want %d", len(nonce), gcm.NonceSize())
	}
	ad, err := randomHex(8)
	if err != nil {
		return "", "", "", err
	}
	sealed := gcm.Seal(nil, []byte(nonce), plaintext, []byte(ad))
	return nonce, ad, base64.StdEncoding.EncodeToString(sealed), nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", buf), nil
}
