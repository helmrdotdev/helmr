package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"
)

const testPasskeyOrigin = "http://localhost"

func testWebAuthn(t *testing.T) *webauthn.WebAuthn {
	t.Helper()
	wa, err := NewWebAuthn(testPasskeyOrigin, "localhost", "Helmr")
	if err != nil {
		t.Fatal(err)
	}
	return wa
}

// testPasskey is a software authenticator holding one discoverable ES256 key
// that verifies its user on every ceremony.
type testPasskey struct {
	id     []byte
	handle []byte
	key    *ecdsa.PrivateKey
	count  uint32
}

func newTestPasskey(t *testing.T) *testPasskey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	if _, err = rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return &testPasskey{id: id, key: key}
}

func encodeTestBytes(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func testClientData(t *testing.T, kind string, challenge []byte) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"type": kind, "challenge": encodeTestBytes(challenge), "origin": testPasskeyOrigin})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// register answers creation options and returns the browser's JSON response.
func (k *testPasskey) register(t *testing.T, options RegistrationOptions) []byte {
	t.Helper()
	switch handle := options.PublicKey.User.ID.(type) {
	case protocol.URLEncodedBase64:
		k.handle = handle
	case []byte:
		k.handle = handle
	default:
		t.Fatalf("unexpected user handle %T", handle)
	}
	public, err := k.key.PublicKey.ECDH()
	if err != nil {
		t.Fatal(err)
	}
	point := public.Bytes()
	cose, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: int64(webauthncose.EllipticKey), Algorithm: int64(webauthncose.AlgES256)},
		Curve:         int64(webauthncose.P256), XCoord: point[1:33], YCoord: point[33:],
	})
	if err != nil {
		t.Fatal(err)
	}
	rp := sha256.Sum256([]byte(options.PublicKey.RelyingParty.ID))
	authData := append(rp[:], 0x01|0x04|0x40)
	authData = binary.BigEndian.AppendUint32(authData, 0)
	authData = append(authData, make([]byte, 16)...)
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(k.id)))
	authData = append(authData, k.id...)
	authData = append(authData, cose...)
	attestation, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(map[string]any{
		"id": encodeTestBytes(k.id), "rawId": encodeTestBytes(k.id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": encodeTestBytes(testClientData(t, "webauthn.create", options.PublicKey.Challenge)), "attestationObject": encodeTestBytes(attestation)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// assert answers discoverable request options with this key.
func (k *testPasskey) assert(t *testing.T, options AssertionOptions) []byte {
	t.Helper()
	rp := sha256.Sum256([]byte(options.PublicKey.RelyingPartyID))
	k.count++
	authData := binary.BigEndian.AppendUint32(append(rp[:], 0x01|0x04), k.count)
	clientData := testClientData(t, "webauthn.get", options.PublicKey.Challenge)
	digest := sha256.Sum256(clientData)
	signed := sha256.Sum256(append(append([]byte{}, authData...), digest[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, k.key, signed[:])
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(map[string]any{
		"id": encodeTestBytes(k.id), "rawId": encodeTestBytes(k.id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": encodeTestBytes(clientData), "authenticatorData": encodeTestBytes(authData), "signature": encodeTestBytes(signature), "userHandle": encodeTestBytes(k.handle)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return response
}
