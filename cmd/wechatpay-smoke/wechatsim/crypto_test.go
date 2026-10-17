package wechatsim

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEncryptResourceRoundTripsThroughStandardGCM locks the notification
// resource layout to what the official SDK actually does: resource.nonce and
// resource.associated_data travel as raw strings and their bytes are passed
// straight to cipher.AEAD.Open. The nonce therefore must be exactly the
// standard GCM nonce size (12 bytes); a base64-encoded nonce of the same byte
// length would be 16 bytes on the wire and would panic the SDK's Open call.
func TestEncryptResourceRoundTripsThroughStandardGCM(t *testing.T) {
	fixture, err := NewFixture()
	require.NoError(t, err)

	plaintext := []byte(`{"out_trade_no":"pay_nonce_roundtrip","mchid":"1900000001"}`)
	nonce, associatedData, ciphertext, err := encryptResource(fixture.APIv3Key, plaintext)
	require.NoError(t, err)
	require.Len(t, nonce, 12, "resource.nonce must be exactly cipher.NewGCM's nonce size as a raw string")

	block, err := aes.NewCipher(fixture.APIv3Key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)

	sealed, err := base64.StdEncoding.DecodeString(ciphertext)
	require.NoError(t, err)
	opened, err := gcm.Open(nil, []byte(nonce), sealed, []byte(associatedData))
	require.NoError(t, err, "the sealed resource must open with a standard GCM using the raw nonce bytes")
	require.Equal(t, plaintext, opened)

	// The associated data is part of the authentication tag.
	_, err = gcm.Open(nil, []byte(nonce), sealed, []byte(associatedData+"x"))
	require.Error(t, err, "a changed associated_data must fail authentication")
}
