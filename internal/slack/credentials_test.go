package slack

import (
	"bytes"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"
)

func TestCredentialEnvelopeBindsOwnerInstallationAndRevision(t *testing.T) {
	store, err := NewCredentialStore(nil, make([]byte, 32))
	if err == nil || store != nil {
		t.Fatal("missing database accepted")
	}
	// Encryption does not access the database; a typed test transaction owner is
	// sufficient to exercise the independent authenticated envelope boundary.
	store, err = NewCredentialStore(&unusedTransactions{}, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	org, installation := uuid.NewV7(), uuid.NewV7()
	bundle := credentialBundle{AccessToken: "private-access"}
	ciphertext, nonce, err := store.seal(org, installation, 1, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte(bundle.AccessToken)) {
		t.Fatal("plaintext custody")
	}
	if got, err := store.open(org, installation, 1, ciphertext, nonce); err != nil || got.AccessToken != bundle.AccessToken {
		t.Fatal("round trip failed")
	}
	for _, c := range []struct {
		org, id           uuid.UUID
		revision          int64
		ciphertext, nonce []byte
	}{
		{uuid.NewV7(), installation, 1, ciphertext, nonce},
		{org, uuid.NewV7(), 1, ciphertext, nonce},
		{org, installation, 2, ciphertext, nonce},
		{org, installation, 1, append([]byte{ciphertext[0] ^ 1}, ciphertext[1:]...), nonce},
		{org, installation, 1, ciphertext, nonce[:1]},
	} {
		if _, err := store.open(c.org, c.id, c.revision, c.ciphertext, c.nonce); err == nil {
			t.Fatal("unbound credential decrypted")
		}
	}
	_, secondNonce, err := store.seal(org, installation, 1, bundle)
	if err != nil || bytes.Equal(nonce, secondNonce) {
		t.Fatal("nonce reused")
	}
}

func TestOAuthExchangeDoesNotReplayOrLeakInvalidResponses(t *testing.T) {
	cases := []struct {
		name, body, code string
		status           int
		retry            string
	}{
		{"rotating", `{"ok":true,"token_type":"bot","access_token":"access","refresh_token":"refresh","expires_in":43200}`, "", 200, ""},
		{"static", `{"ok":true,"token_type":"bot","access_token":"access"}`, "oauth_response_invalid", 200, ""},
		{"missing-expiry", `{"ok":true,"token_type":"bot","access_token":"access","refresh_token":"refresh"}`, "oauth_response_invalid", 200, ""},
		{"missing-refresh", `{"ok":true,"token_type":"bot","access_token":"access","expires_in":43200}`, "oauth_response_invalid", 200, ""},
		{"duplicate", `{"ok":true,"ok":false,"access_token":"private"}`, "oauth_response_invalid", 200, ""},
		{"user-token", `{"ok":true,"token_type":"user","access_token":"private"}`, "oauth_response_invalid", 200, ""},
		{"rejected", `{"ok":false,"error":"invalid_refresh_token"}`, "oauth_credential_rejected", 200, ""},
		{"unknown-error", `{"ok":false,"error":"private-server-text"}`, "oauth_outcome_unknown", 200, ""},
		{"server", `private-server-text`, "oauth_outcome_unknown", 503, ""},
		{"rate", `private-server-text`, "oauth_rate_limited", 429, "7"},
		{"rate-no-instruction", `private-server-text`, "oauth_outcome_unknown", 429, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			oauth, err := NewOAuthClient("client", "client-secret", roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				user, password, ok := r.BasicAuth()
				if !ok || user != "client" || password != "client-secret" || r.GetBody != nil || r.URL.String() != "https://slack.com/api/oauth.v2.access" {
					t.Fatal("unsafe exchange request")
				}
				if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" {
					t.Fatal("invalid exchange form")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": {tc.retry}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			result := oauth.exchange(t.Context(), map[string][]string{"grant_type": {"refresh_token"}, "refresh_token": {"old-refresh"}})
			if calls != 1 || result.code != tc.code {
				t.Fatalf("calls=%d code=%s", calls, result.code)
			}
			if tc.name == "rate" && result.retryAfter != 7*time.Second {
				t.Fatal("retry instruction lost")
			}
		})
	}
}

type unusedTransactions struct{}

func (*unusedTransactions) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("unexpected database use")
}
