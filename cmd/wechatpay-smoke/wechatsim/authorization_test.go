package wechatsim

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/credentials"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/signers"
)

// TestAuthorizationHeaderRoundTripsOfficialSDKSigner locks the wire contract the
// simulator's request verification depends on: the header the official SDK's own
// credential generator produces must parse and verify against the fake merchant
// key, and a single changed byte of the canonical string must not.
//
// This is the regression guard for a parser that split the comma-separated
// parameter list on whitespace, which made every real request look unsigned.
func TestAuthorizationHeaderRoundTripsOfficialSDKSigner(t *testing.T) {
	fixture, err := NewFixture()
	require.NoError(t, err)

	signer := &signers.SHA256WithRSASigner{
		MchID:               fixture.MerchantID,
		CertificateSerialNo: fixture.MerchantSerialNo,
		PrivateKey:          fixture.MerchantPrivateKey,
	}
	credential := &credentials.WechatPayCredentials{Signer: signer}

	const (
		method       = http.MethodPost
		canonicalURL = "/v3/pay/transactions/native"
	)
	body := `{"appid":"` + fixture.AppID + `","mchid":"` + fixture.MerchantID + `","out_trade_no":"pay_roundtrip","amount":{"total":10000,"currency":"CNY"}}`

	header, err := credential.GenerateAuthorizationHeader(context.Background(), method, canonicalURL, body)
	require.NoError(t, err)

	parts, err := ParseAuthorizationHeader(header)
	require.NoError(t, err, "the SDK-produced header must parse; got %q", header)
	require.Equal(t, fixture.MerchantID, parts.MchID)
	require.Equal(t, fixture.MerchantSerialNo, parts.SerialNo)
	require.NotEmpty(t, parts.Nonce)
	require.NotEmpty(t, parts.Timestamp)

	message := RequestSignatureMessage(method, canonicalURL, parts.Timestamp, parts.Nonce, body)
	require.NoError(t, VerifySHA256WithRSA(message, parts.Signature, fixture.MerchantPublicKey()),
		"the SDK signature must verify over the canonical request string")

	// The exact canonical string is part of the proof.
	require.Error(t, VerifySHA256WithRSA(
		RequestSignatureMessage(method, canonicalURL, parts.Timestamp, parts.Nonce, body+"x"),
		parts.Signature, fixture.MerchantPublicKey()), "a tampered body must not verify")
	require.Error(t, VerifySHA256WithRSA(
		RequestSignatureMessage(method, canonicalURL+"?mchid="+fixture.MerchantID, parts.Timestamp, parts.Nonce, body),
		parts.Signature, fixture.MerchantPublicKey()), "a changed canonical URL must not verify")
}
