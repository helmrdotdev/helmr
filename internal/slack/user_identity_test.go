package slack

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestSlackUserIdentityVerifiesSignedOpenIDClaims(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"valid", "missing_key_use", "encryption_key_use", "wrong_nonce", "wrong_team", "different_subject", "missing_subject", "wrong_issuer", "wrong_audience", "multiple_audiences", "expired", "missing_expiry", "missing_issued", "future_issued", "wrong_key", "hmac", "unknown_kid", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			claims := jwt.MapClaims{"iss": "https://slack.com", "sub": "U1", "aud": "client", "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Add(-time.Second).Unix(), "nonce": "nonce", "https://slack.com/team_id": "T1", "https://slack.com/user_id": "U1", "name": "Slack person", "email": "never-match@example.test"}
			switch kind {
			case "wrong_nonce":
				claims["nonce"] = "other"
			case "wrong_team":
				claims["https://slack.com/team_id"] = "T2"
			case "different_subject":
				claims["sub"] = "opaque-subject"
			case "missing_subject":
				delete(claims, "sub")
			case "wrong_issuer":
				claims["iss"] = "https://other.test"
			case "wrong_audience":
				claims["aud"] = "other"
			case "multiple_audiences":
				claims["aud"] = []string{"client", "other"}
			case "expired":
				claims["exp"] = time.Now().Add(-time.Minute).Unix()
			case "missing_expiry":
				delete(claims, "exp")
			case "missing_issued":
				delete(claims, "iat")
			case "future_issued":
				claims["iat"] = time.Now().Add(time.Hour).Unix()
			}
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			token.Header["kid"] = "key"
			var signing any = key
			if kind == "hmac" {
				token = jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
				token.Header["kid"] = "key"
				signing = []byte("wrong-key")
			}
			if kind == "unknown_kid" {
				token.Header["kid"] = "other"
			}
			if kind == "wrong_key" {
				signing, err = rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatal(err)
				}
			}
			signed, err := token.SignedString(signing)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			client, err := NewOAuthClient("client", "client-secret", roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "slack.com" {
					t.Fatal("redirect followed", r.URL)
				}
				var value any
				switch r.URL.Path {
				case "/api/openid.connect.token":
					if r.Method != "POST" || r.GetBody != nil {
						t.Fatal("exchange can replay")
					}
					user, password, ok := r.BasicAuth()
					if !ok || user != "client" || password != "client-secret" {
						t.Fatal("incorrect client authentication")
					}
					if err := r.ParseForm(); err != nil || r.PostForm.Get("code") != "code" || r.PostForm.Get("redirect_uri") != "https://console.test/callback" {
						t.Fatal("incorrect exchange")
					}
					if kind == "redirect" {
						return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://other.test/steal"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
					}
					value = map[string]any{"ok": true, "id_token": signed, "access_token": "not-retained"}
				case "/openid/connect/keys":
					if r.Method != "GET" || r.Header.Get("Authorization") != "" {
						t.Fatal("credential forwarded to key endpoint")
					}
					jwk := map[string]string{"kid": "key", "kty": "RSA", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}
					if kind == "missing_key_use" {
						delete(jwk, "use")
					}
					if kind == "encryption_key_use" {
						jwk["use"] = "enc"
					}
					value = map[string]any{"keys": []any{jwk}}
				default:
					t.Fatal("unexpected endpoint", r.URL)
				}
				body, _ := json.Marshal(value)
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			identity, err := client.VerifyIdentity(t.Context(), "code", "https://console.test/callback", "nonce", "T1")
			if kind == "valid" || kind == "different_subject" || kind == "missing_key_use" {
				if err != nil || identity.TeamID != "T1" || identity.SlackUserID != "U1" || identity.Name != "Slack person" {
					t.Fatal(identity, err)
				}
			} else if !errors.Is(err, ErrUserIdentity) || identity != (UserIdentity{}) {
				t.Fatal("invalid proof accepted", identity, err)
			}
			if calls > 2 {
				t.Fatal("exchange replayed")
			}
		})
	}
}
func TestSlackIdentityAuthorizationIsSeparateFromInstallation(t *testing.T) {
	client, err := NewOAuthClient("client", "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := client.IdentityAuthorizationURL("state", "nonce", "team", "https://console.test/callback")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if parsed.Host != "slack.com" || parsed.Path != "/openid/connect/authorize" || q.Get("scope") != "openid profile" || q.Get("response_mode") != "form_post" || q.Get("state") != "state" || q.Get("nonce") != "nonce" || q.Get("team") != "team" {
		t.Fatal(raw)
	}
}
