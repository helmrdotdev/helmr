package sso

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// testOIDCIdP is a local issuer that signs ID tokens with chosen claims, so
// vendor claim shapes can be exercised without a vendor account.
type testOIDCIdP struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	// signer overrides how the token endpoint signs ID tokens.
	signer func(t *testing.T, claims map[string]any) string
	claims func(nonce string) map[string]any
	nonce  string
}

func newTestOIDCIdP(t *testing.T) *testOIDCIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &testOIDCIdP{key: key}
	mux := http.NewServeMux()
	idp.server = httptest.NewTLSServer(mux)
	t.Cleanup(idp.server.Close)
	issuer := idp.server.URL
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
		sign := idp.sign
		if idp.signer != nil {
			sign = idp.signer
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": sign(t, idp.claims(idp.nonce))})
	})
	return idp
}

func (i *testOIDCIdP) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: i.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
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

func (i *testOIDCIdP) client() *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(i.server.Certificate())
	return newHTTPClient(true, pool)
}

func TestOIDCVendorClaimShapes(t *testing.T) {
	idp := newTestOIDCIdP(t)
	now := time.Now().Truncate(time.Second)
	base := func(nonce string) map[string]any {
		return map[string]any{"iss": idp.server.URL, "aud": "helmr", "sub": "user-1", "nonce": nonce,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "email": "alice@example.test"}
	}
	for _, tc := range []struct {
		name     string
		edit     func(map[string]any)
		wantTime time.Time
		wantErr  bool
	}{
		{"auth_time present (Okta, Keycloak)", func(c map[string]any) { c["auth_time"] = now.Add(-time.Hour).Unix(); c["azp"] = "helmr" }, now.Add(-time.Hour), false},
		{"no auth_time falls back to iat (Entra default)", func(c map[string]any) {
			c["tid"], c["oid"] = "tenant", "object"
		}, now, false},
		{"string audience list with authorized party", func(c map[string]any) { c["aud"] = []string{"helmr", "other"}; c["azp"] = "helmr" }, now, false},
		{"issued to another client", func(c map[string]any) { c["azp"] = "other" }, time.Time{}, true},
		{"multiple audiences without authorized party", func(c map[string]any) { c["aud"] = []string{"helmr", "other"} }, time.Time{}, true},
		{"wrong audience", func(c map[string]any) { c["aud"] = "other" }, time.Time{}, true},
		{"wrong issuer", func(c map[string]any) { c["iss"] = "https://evil.example.test" }, time.Time{}, true},
		{"expired", func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() }, time.Time{}, true},
		{"authentication in the future", func(c map[string]any) { c["auth_time"] = now.Add(time.Hour).Unix() }, time.Time{}, true},
		{"wrong nonce", func(c map[string]any) { c["nonce"] = "other" }, time.Time{}, true},
		{"zero auth_time is not replaced by iat", func(c map[string]any) { c["auth_time"] = 0 }, time.Time{}, true},
		{"negative auth_time", func(c map[string]any) { c["auth_time"] = -1 }, time.Time{}, true},
		{"null auth_time", func(c map[string]any) { c["auth_time"] = nil }, time.Time{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idp.nonce = "n-" + tc.name
			idp.claims = func(nonce string) map[string]any { c := base(nonce); tc.edit(c); return c }
			o, err := NewOIDC(context.Background(), idp.client(), idp.server.URL, "helmr", "secret", "https://helmr.example.test/sso/oidc/callback")
			if err != nil {
				t.Fatal(err)
			}
			got, err := o.Exchange(context.Background(), "good-code", "verifier-verifier-verifier-verifier-verifier", idp.nonce)
			if tc.wantErr {
				if !errors.Is(err, ErrOIDCToken) {
					t.Fatalf("err = %v, want refusal (identity %+v)", err, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Subject != "user-1" || got.Email != "alice@example.test" || !got.AuthenticatedAt.Equal(tc.wantTime) {
				t.Fatalf("identity = %+v, want time %v", got, tc.wantTime)
			}
		})
	}
}

func TestOIDCAuthURLAndDiscovery(t *testing.T) {
	idp := newTestOIDCIdP(t)
	o, err := NewOIDC(context.Background(), idp.client(), idp.server.URL, "helmr", "secret", "https://helmr.example.test/cb")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		reauth bool
		prompt string
		maxAge string
	}{{false, "", "2592000"}, {true, "login", "0"}} {
		u, _ := url.Parse(o.AuthURL("state", "nonce", "verifier-verifier-verifier-verifier-verifier", tc.reauth))
		q := u.Query()
		if q.Get("prompt") != tc.prompt || q.Get("max_age") != tc.maxAge || q.Get("nonce") != "nonce" || q.Get("code_challenge_method") != "S256" || !strings.Contains(q.Get("scope"), "openid") {
			t.Fatalf("reauth=%v query %v", tc.reauth, q)
		}
	}
	// The issuer must equal the discovery document exactly (Entra's /common
	// issuer cannot pass; organizations use their tenant issuer).
	if _, err := NewOIDC(context.Background(), idp.client(), idp.server.URL+"/", "helmr", "secret", "https://x/cb"); err == nil {
		t.Fatal("trailing-slash issuer accepted")
	}
	if _, err := NewOIDC(context.Background(), idp.client(), strings.Replace(idp.server.URL, "https", "http", 1), "helmr", "secret", "https://x/cb"); err == nil {
		t.Fatal("http issuer accepted")
	}
}

func TestOutboundClientRefusesPrivateDestinationsOnCloud(t *testing.T) {
	idp := newTestOIDCIdP(t)
	pool := x509.NewCertPool()
	pool.AddCert(idp.server.Certificate())
	if _, err := newHTTPClient(false, pool).Get(idp.server.URL + "/keys"); !errors.Is(err, errPrivateDestination) {
		t.Fatalf("loopback destination err = %v", err)
	}
	if resp, err := newHTTPClient(true, pool).Get(idp.server.URL + "/keys"); err != nil {
		t.Fatalf("self-host loopback err = %v", err)
	} else {
		resp.Body.Close()
	}
	if _, err := newHTTPClient(true, pool).Get(strings.Replace(idp.server.URL, "https", "http", 1)); err == nil {
		t.Fatal("http request allowed")
	}
}

func TestOIDCRefusesForeignSignatures(t *testing.T) {
	idp := newTestOIDCIdP(t)
	now := time.Now()
	idp.nonce = "n"
	idp.claims = func(nonce string) map[string]any {
		return map[string]any{"iss": idp.server.URL, "aud": "helmr", "sub": "user-1", "nonce": nonce, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, signer := range map[string]func(*testing.T, map[string]any) string{
		"unknown key with the published key ID": func(t *testing.T, c map[string]any) string {
			return signWith(t, jose.SigningKey{Algorithm: jose.RS256, Key: other}, c)
		},
		"HS256 keyed with the client secret": func(t *testing.T, c map[string]any) string {
			return signWith(t, jose.SigningKey{Algorithm: jose.HS256, Key: []byte("secret-secret-secret-secret-secret")}, c)
		},
	} {
		t.Run(name, func(t *testing.T) {
			idp.signer = signer
			o, err := NewOIDC(context.Background(), idp.client(), idp.server.URL, "helmr", "secret-secret-secret-secret-secret", "https://x/cb")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := o.Exchange(context.Background(), "good-code", "verifier-verifier-verifier-verifier-verifier", "n"); !errors.Is(err, ErrOIDCToken) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func signWith(t *testing.T, key jose.SigningKey, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(key, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
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

func TestPublicIPRefusesNonPublicRanges(t *testing.T) {
	for _, ip := range []string{"10.1.2.3", "100.64.0.1", "0.1.2.3", "127.0.0.1", "169.254.169.254", "192.0.0.8", "198.18.0.1",
		"240.0.0.1", "255.255.255.255", "::1", "fd00::1", "fe80::1", "64:ff9b::a9fe:a9fe", "2002:a9fe:a9fe::1", "2001::1", "::ffff:10.0.0.1"} {
		if publicIP(net.ParseIP(ip)) {
			t.Errorf("%s treated as public", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(net.ParseIP(ip)) {
			t.Errorf("%s treated as non-public", ip)
		}
	}
}
