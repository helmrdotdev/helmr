package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestGitHubOAuthProviderUsesGitHubAuthCallback(t *testing.T) {
	publicURL, err := url.Parse("https://helmr.example.com")
	if err != nil {
		t.Fatal(err)
	}
	provider := NewGitHubOAuthProvider("client-id", "client-secret", publicURL)

	redirectURL, err := url.Parse(provider.RedirectURL("state", "verifier"))
	if err != nil {
		t.Fatalf("redirect URL is invalid: %v", err)
	}

	if got := redirectURL.Query().Get("redirect_uri"); got != "https://helmr.example.com/auth/github/callback" {
		t.Fatalf("redirect_uri = %q", got)
	}
	if got := redirectURL.Query().Get("scope"); got != "user:email" {
		t.Fatalf("scope = %q", got)
	}
}

func TestGitHubOAuthProviderFallsBackToPrimaryVerifiedEmail(t *testing.T) {
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			writeJSON(w, http.StatusOK, map[string]string{
				"access_token": "token",
				"token_type":   "bearer",
			})
		case "/user":
			writeJSON(w, http.StatusOK, map[string]any{
				"id":         123,
				"login":      "octocat",
				"email":      "",
				"avatar_url": "https://avatars.githubusercontent.com/u/123?v=4",
			})
		case "/user/emails":
			writeJSON(w, http.StatusOK, []map[string]any{
				{"email": "other@example.test", "primary": false, "verified": true},
				{"email": "owner@example.test", "primary": true, "verified": true},
			})
		default:
			t.Fatalf("unexpected path %s on %s", r.URL.Path, serverURL)
		}
	}))
	defer server.Close()
	serverURL = server.URL

	publicURL, err := url.Parse("https://helmr.example.com")
	if err != nil {
		t.Fatal(err)
	}
	provider := NewGitHubOAuthProvider("client-id", "client-secret", publicURL).(*githubOAuthProvider)
	provider.config.Endpoint.TokenURL = server.URL + "/token"
	provider.userURL = server.URL + "/user"
	provider.userEmailsURL = server.URL + "/user/emails"

	identity, _, err := provider.ResolveWithToken(t.Context(), "code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Email != "owner@example.test" {
		t.Fatalf("email = %q", identity.Email)
	}
	if !identity.EmailVerified {
		t.Fatal("email was not marked verified")
	}
	if len(identity.VerifiedEmails) != 2 {
		t.Fatalf("verified emails = %v", identity.VerifiedEmails)
	}
	if identity.ProfileImageURL != "https://avatars.githubusercontent.com/u/123?v=4" {
		t.Fatalf("profile image URL = %q", identity.ProfileImageURL)
	}

	var claims map[string]string
	if err := json.Unmarshal(identity.Claims, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["login"] != "octocat" {
		t.Fatalf("claims = %+v", claims)
	}
	if _, ok := claims["avatar_url"]; ok {
		t.Fatalf("claims should not duplicate profile image URL: %+v", claims)
	}
}

func TestGitHubOAuthProviderAllowsMissingPrivateEmail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			writeJSON(w, http.StatusOK, map[string]string{
				"access_token": "token",
				"token_type":   "bearer",
			})
		case "/user":
			writeJSON(w, http.StatusOK, map[string]any{
				"id":    123,
				"login": "octocat",
				"email": "",
			})
		case "/user/emails":
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	publicURL, err := url.Parse("https://helmr.example.com")
	if err != nil {
		t.Fatal(err)
	}
	provider := NewGitHubOAuthProvider("client-id", "client-secret", publicURL).(*githubOAuthProvider)
	provider.config.Endpoint.TokenURL = server.URL + "/token"
	provider.userURL = server.URL + "/user"
	provider.userEmailsURL = server.URL + "/user/emails"

	identity, _, err := provider.ResolveWithToken(t.Context(), "code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Email != "" {
		t.Fatalf("email = %q", identity.Email)
	}
	if identity.EmailVerified || len(identity.VerifiedEmails) != 0 {
		t.Fatalf("verified email state = %v %v", identity.EmailVerified, identity.VerifiedEmails)
	}
	if identity.EmailLookupErr == "" {
		t.Fatal("expected email lookup error")
	}
}

func TestGitHubOAuthProviderUsesPrimaryVerifiedEmailOverPublicEmail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			writeJSON(w, http.StatusOK, map[string]string{
				"access_token": "token",
				"token_type":   "bearer",
			})
		case "/user":
			writeJSON(w, http.StatusOK, map[string]any{
				"id":    123,
				"login": "octocat",
				"email": "public@example.test",
			})
		case "/user/emails":
			writeJSON(w, http.StatusOK, []map[string]any{
				{"email": "owner@example.test", "primary": true, "verified": true},
				{"email": "public@example.test", "primary": false, "verified": true},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	publicURL, err := url.Parse("https://helmr.example.com")
	if err != nil {
		t.Fatal(err)
	}
	provider := NewGitHubOAuthProvider("client-id", "client-secret", publicURL).(*githubOAuthProvider)
	provider.config.Endpoint.TokenURL = server.URL + "/token"
	provider.userURL = server.URL + "/user"
	provider.userEmailsURL = server.URL + "/user/emails"

	identity, _, err := provider.ResolveWithToken(t.Context(), "code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Email != "owner@example.test" || !identity.EmailVerified {
		t.Fatalf("email = %q verified=%v", identity.Email, identity.EmailVerified)
	}
}
