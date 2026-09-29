package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/identity"
)

func TestIdentityErrorMapsHTTPContract(t *testing.T) {
	_, input := identity.ParseAPIKeyFilter("pending")
	for _, test := range []struct {
		err     error
		status  int
		code    string
		message string
	}{
		{input, http.StatusBadRequest, "bad_request", input.Error()},
		{identity.ErrInvalidDeviceCode, http.StatusBadRequest, "bad_request", "invalid device code"},
		{identity.ErrInvalidToken, http.StatusBadRequest, "invalid_token", "token is invalid or expired"},
		{identity.ErrWrongAccount, http.StatusBadRequest, "wrong_account", "verified email does not match invitation"},
		{identity.ErrAlreadyMember, http.StatusConflict, "already_member", "identity is already a member of this organization"},
		{identity.ErrInactiveMember, http.StatusConflict, "disabled_member", "membership is no longer active"},
		{identity.ErrUserNotFound, http.StatusUnauthorized, "unauthorized", "authentication is required"},
		{identity.ErrOrganizationRequired, http.StatusForbidden, "forbidden", "organization is required"},
		{identity.ErrAPIKeyManagementRequired, http.StatusForbidden, "forbidden", identity.ErrAPIKeyManagementRequired.Error()},
		{identity.ErrDeviceCodeNotFound, http.StatusNotFound, "not_found", "device code not found"},
		{identity.ErrAPIKeyNotFound, http.StatusNotFound, "not_found", "api key not found"},
		{identity.ErrDeviceApproverChanged, http.StatusConflict, "device_identity_changed", "Your signed-in account or organization changed. Review the current account before approving."},
		{fmt.Errorf("record auth identity: %w", errors.New("connection reset")), http.StatusInternalServerError, "internal_error", "internal server error"},
	} {
		t.Run(test.err.Error(), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeError(recorder, identityError(test.err))
			var body api.HTTPErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != test.status || body.Error.Code != test.code || body.Error.Message != test.message {
				t.Fatalf("response = %d %+v, want %d %s %q", recorder.Code, body.Error, test.status, test.code, test.message)
			}
		})
	}
}

type continuationAuthProvider struct{}

func (continuationAuthProvider) RedirectURL(state, verifier string) string {
	return "https://github.example.test/authorize?state=" + url.QueryEscape(state)
}

func (continuationAuthProvider) Resolve(context.Context, string, string) (identity.ExternalIdentity, error) {
	return identity.ExternalIdentity{Provider: "github", Subject: "continuation", DisplayName: "Fixture user", Email: "fixture@example.test", EmailVerified: true}, nil
}

func TestBrowserAuthSupersededCallbackKeepsNewerFlow(t *testing.T) {
	cfg := completeServerConfig(t)
	cfg.PublicURL = &url.URL{Scheme: "https", Host: "helmr.example.test"}
	cfg.AuthProvider = continuationAuthProvider{}
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(api.GitHubAuthStartRequest{Next: "/auth/device?code=NEW"})
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, httptest.NewRequest("POST", "https://helmr.example.test/api/auth/github/start", bytes.NewReader(payload)))
	var started api.GitHubAuthStartResponse
	if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &started) != nil {
		t.Fatalf("start: %d %s", start.Code, start.Body.String())
	}
	redirect, err := url.Parse(started.RedirectURL)
	if err != nil {
		t.Fatal(err)
	}
	newerState := redirect.Query().Get("state")
	for _, state := range []string{"older-state", "", newerState} {
		for _, denied := range []bool{false, true} {
			body := map[string]string{"state": state}
			if denied {
				body["error"] = "access_denied"
			}
			raw, _ := json.Marshal(body)
			r := httptest.NewRequest("POST", "https://helmr.example.test/api/auth/github/finish", bytes.NewReader(raw))
			for _, cookie := range start.Result().Cookies() {
				r.AddCookie(cookie)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("state=%q denied=%v status=%d", state, denied, w.Code)
			}
			cleared := false
			for _, cookie := range w.Result().Cookies() {
				if cookie.Name == authFlowCookieName(r) && cookie.MaxAge < 0 {
					cleared = true
				}
			}
			if cleared != (state == newerState) {
				t.Fatalf("state=%q denied=%v cleared=%v", state, denied, cleared)
			}
		}
	}
}

func TestBrowserAuthReturnDestinationValidation(t *testing.T) {
	for _, value := range []string{"https://evil.test", "//evil.test", "/\\evil.test", "/bad\npath", "/bad\x00path"} {
		if got := validateRedirectAfter(value); got != "/" {
			t.Fatalf("accepted %q: %q", value, got)
		}
	}
	const destination = "/auth/device?code=ABCD-EFGH#confirm"
	if got := validateRedirectAfter(destination); got != destination {
		t.Fatalf("lost destination %q", got)
	}
}
