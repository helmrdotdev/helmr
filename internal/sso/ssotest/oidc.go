package ssotest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
)

// OIDCIssuer is a local issuer that signs ID tokens with chosen claims, so
// vendor claim shapes can be exercised without a vendor account.
type OIDCIssuer struct {
	Server *httptest.Server
	Key    *rsa.PrivateKey
	// Signer overrides how the token endpoint signs ID tokens. Claims builds
	// them from the nonce Helmr sent, which tests set in Nonce.
	Signer func(t *testing.T, claims map[string]any) string
	Claims func(nonce string) map[string]any
	Nonce  string
}

func NewOIDCIssuer(t *testing.T) *OIDCIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &OIDCIssuer{Key: key}
	mux := http.NewServeMux()
	idp.Server = httptest.NewTLSServer(mux)
	t.Cleanup(idp.Server.Close)
	issuer := idp.Server.URL
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token",
			"jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("code") != "good-code" || r.FormValue("code_verifier") == "" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		sign := idp.Sign
		if idp.Signer != nil {
			sign = idp.Signer
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": sign(t, idp.Claims(idp.Nonce))})
	})
	return idp
}

// Sign signs claims with the issuer's published key.
func (i *OIDCIssuer) Sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: i.Key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
