package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/identity"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestAuthContinuationUsesExplicitOrganization(t *testing.T) {
	f := newHTTPPostgresFixture(t, func(cfg *ServerConfig) { cfg.DeploymentMode = deploymentModeManagedCloud })
	user := f.user(t, "Developer")
	orgA, orgB := uuid.NewV7(), uuid.NewV7()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO organizations(id,name,slug) VALUES($1,'First','first'),($2,'Second','second')`, orgA, orgB); err != nil {
		t.Fatal(err)
	}
	f.member(t, orgA, user, "owner")
	f.member(t, orgB, user, "member")
	token := f.session(t, user, orgB)
	w := f.request(t, http.MethodGet, "/api/me", token, "")
	var account api.MeResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &account) != nil || account.UserID != user.String() {
		t.Fatalf("account: %d %s", w.Code, w.Body)
	}
	for _, field := range []string{`"org_id"`, `"permissions"`, `"is_owner"`} {
		if strings.Contains(w.Body.String(), field) {
			t.Fatalf("organization leaked into account: %s", w.Body)
		}
	}
	contextRead := func(raw string) (int, api.OrganizationContextResponse) {
		t.Helper()
		w := f.request(t, http.MethodGet, "/api/organization", raw, "")
		var result api.OrganizationContextResponse
		if w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal(w.Body.String())
		}
		return w.Code, result
	}
	if status, result := contextRead(token); status != 200 || result.ID != orgB.String() || result.CurrentMember.IsOwner {
		t.Fatalf("selected organization: %d %+v", status, result)
	}
	unselected := f.session(t, user, uuid.Nil())
	if status, _ := contextRead(unselected); status == 200 {
		t.Fatal("silently selected first organization")
	}
	if w := f.request(t, http.MethodGet, "/api/me", unselected, ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	start := f.request(t, http.MethodPost, "/api/auth/device/start", "", "")
	var device api.DeviceStartResponse
	if start.Code != 201 || json.Unmarshal(start.Body.Bytes(), &device) != nil {
		t.Fatalf("start: %s", start.Body)
	}
	if device.VerificationURIComplete != "https://console.example.test/auth/device?code="+url.QueryEscape(device.UserCode) {
		t.Fatal(device.VerificationURIComplete)
	}
	exchange := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(api.DeviceTokenRequest{DeviceCode: device.DeviceCode})
		return f.request(t, http.MethodPost, "/api/auth/device/token", "", string(body))
	}
	if w := exchange(); w.Code != 202 || !strings.Contains(w.Body.String(), "authorization_pending") {
		t.Fatalf("pending: %d %s", w.Code, w.Body)
	}
	statusRead := f.request(t, http.MethodGet, "/api/auth/device/status?user_code="+url.QueryEscape(device.UserCode), token, "")
	var status api.DeviceStatusResponse
	if statusRead.Code != 200 || json.Unmarshal(statusRead.Body.Bytes(), &status) != nil || status.Consent == nil {
		t.Fatalf("status: %d %s", statusRead.Code, statusRead.Body)
	}
	consentHeader := http.Header{"X-Helmr-Device-Consent": []string{status.Consent.Proof}}
	body, _ := json.Marshal(api.DeviceAuthorizeRequest{UserCode: device.UserCode})
	// The consent proof binds the account, organization and code shown; an
	// altered proof is refused.
	other := http.Header{"X-Helmr-Device-Consent": []string{status.Consent.Proof + "x"}}
	if w := f.request(t, http.MethodPost, "/api/auth/device/approve", token, string(body), other); w.Code != 409 {
		t.Fatalf("changed selection: %d %s", w.Code, w.Body)
	}
	if w := f.request(t, http.MethodPost, "/api/auth/device/approve", token, string(body), consentHeader); w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	w = exchange()
	var issued api.DeviceTokenResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &issued) != nil || issued.AccessToken == "" {
		t.Fatalf("exchange: %d %s", w.Code, w.Body)
	}
	if status, result := contextRead(issued.AccessToken); status != 200 || result.ID != orgB.String() {
		t.Fatalf("CLI context: %d %+v", status, result)
	}
	if w := f.request(t, http.MethodGet, "/api/me", issued.AccessToken, ""); w.Code == 200 {
		t.Fatal("CLI entered account API")
	}
	if w := exchange(); w.Code != 400 {
		t.Fatalf("replay: %d", w.Code)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE org_members SET removed_at=clock_timestamp() WHERE org_id=$1 AND user_id=$2`, orgB, user); err != nil {
		t.Fatal(err)
	}
	if status, _ := contextRead(token); status == 200 {
		t.Fatal("removed membership admitted")
	}
	if status, _ := contextRead(issued.AccessToken); status == 200 {
		t.Fatal("removed CLI membership admitted")
	}
	if w := f.request(t, http.MethodGet, "/api/me", token, ""); w.Code != 200 {
		t.Fatal("organization removal destroyed account")
	}
	f.browserOrganizations[token] = orgA
	if status, result := contextRead(token); status != 200 || result.ID != orgA.String() {
		t.Fatalf("other membership: %d %+v", status, result)
	}
	newcomer := f.user(t, "Newcomer")
	newToken := f.session(t, newcomer, uuid.Nil())
	if w := f.request(t, http.MethodGet, "/api/organizations", newToken, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"organizations":[]`) {
		t.Fatalf("onboarding: %d %s", w.Code, w.Body)
	}
	if w := f.request(t, http.MethodPost, "/api/auth/logout", token, ""); w.Code != 204 {
		t.Fatalf("logout: %d %s", w.Code, w.Body)
	}
	if w := f.request(t, http.MethodGet, "/api/me", token, ""); w.Code != 403 {
		t.Fatalf("logged out: %d", w.Code)
	}
}

func TestAuthContinuationProvidersReturnStoredDestination(t *testing.T) {
	fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		cfg.AppOrigin = &url.URL{Scheme: "https", Host: "helmr.example.test"}
		cfg.AuthProvider = continuationAuthProvider{}
	})
	const next = "/auth/device?code=ABCD-EFGH#confirm"
	serve := func(r *http.Request) *httptest.ResponseRecorder {
		r.Header.Set("Origin", "https://helmr.example.test")
		w := httptest.NewRecorder()
		fixture.handler.ServeHTTP(w, r)
		return w
	}
	payload, _ := json.Marshal(api.GitHubAuthStartRequest{Next: next})
	start := serve(httptest.NewRequest(http.MethodPost, "https://helmr.example.test/api/auth/github/start", bytes.NewReader(payload)))
	var started api.GitHubAuthStartResponse
	if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &started) != nil {
		t.Fatalf("start: %d %s", start.Code, start.Body.String())
	}
	redirect, err := url.Parse(started.RedirectURL)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ = json.Marshal(api.GitHubAuthFinishRequest{State: redirect.Query().Get("state"), Code: "fixture"})
	r := httptest.NewRequest(http.MethodPost, "https://helmr.example.test/api/auth/github/finish", bytes.NewReader(payload))
	for _, cookie := range start.Result().Cookies() {
		r.AddCookie(cookie)
	}
	finish := serve(r)
	var finished api.GitHubAuthFinishResponse
	if finish.Code != http.StatusOK || json.Unmarshal(finish.Body.Bytes(), &finished) != nil || finished.RedirectAfter != next {
		t.Fatalf("GitHub lost next: %d %s", finish.Code, finish.Body.String())
	}
	if !hasSessionCookie(finish, r) {
		t.Fatal("GitHub sign-in set no session cookie")
	}

}

func hasSessionCookie(w *httptest.ResponseRecorder, r *http.Request) bool {
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == sessionCookieName(r) && cookie.Value != "" && cookie.MaxAge == int((400*24*time.Hour).Seconds()) {
			return true
		}
	}
	return false
}

func TestAuthContinuationEmailCodeIsBrowserBoundAndReturnsDestination(t *testing.T) {
	sender := &recordingEmailCodeSender{sent: make(chan email.Message, 1)}
	delivery := newEmailCodeDelivery(discardEmailCodeLog(), 1, 1)
	f := newEmailCodeDeliveryPostgresFixture(t, sender, delivery)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- delivery.Run(ctx) }()
	defer func() { cancel(); <-done }()
	const next = "/auth/device?code=ABCD-EFGH#confirm"
	start := f.request(t, http.MethodPost, "/api/auth/email/start", "", `{"email":"browser@example.test","next":"`+next+`"}`)
	var challenge identity.EmailChallenge
	if start.Code != 202 || json.Unmarshal(start.Body.Bytes(), &challenge) != nil {
		t.Fatalf("start: %d %s", start.Code, start.Body)
	}
	var message email.Message
	select {
	case message = <-sender.sent:
	case <-time.After(time.Second):
		t.Fatal("no email")
	}
	code := regexp.MustCompile(`[0-9]{8}`).FindString(message.PlainText)
	if code == "" || strings.Contains(start.Body.String(), code) {
		t.Fatal("code missing or disclosed in response")
	}
	deadline := time.Now().Add(time.Second)
	for {
		var sent bool
		if err := f.pool.QueryRow(t.Context(), `SELECT sent_at IS NOT NULL FROM email_challenges WHERE id=$1`, challenge.ID).Scan(&sent); err != nil {
			t.Fatal(err)
		}
		if sent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delivery acknowledgement missing")
		}
		time.Sleep(time.Millisecond)
	}
	verify := func(bound bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/email/"+challenge.ID.String()+"/verify", strings.NewReader(`{"code":"`+code+`"}`))
		r.Header.Set("Origin", f.origin)
		if bound {
			for _, c := range start.Result().Cookies() {
				r.AddCookie(c)
			}
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	if w := verify(false); w.Code != 400 {
		t.Fatalf("unbound verification: %d %s", w.Code, w.Body)
	}
	finish := verify(true)
	var result api.AuthenticationFinishResponse
	if finish.Code != 200 || json.Unmarshal(finish.Body.Bytes(), &result) != nil || result.RedirectAfter != next {
		t.Fatalf("finish: %d %s", finish.Code, finish.Body)
	}
	if !hasSessionCookie(finish, httptest.NewRequest(http.MethodPost, "/", nil)) {
		t.Fatal("no browser session")
	}
	if w := verify(true); w.Code != 400 {
		t.Fatalf("replayed verification: %d %s", w.Code, w.Body)
	}
}

func TestDeviceConsentKeepsDisplayedAccountAndBrowser(t *testing.T) {
	f := newHTTPPostgresFixture(t, func(cfg *ServerConfig) { cfg.DeploymentMode = deploymentModeManagedCloud })
	first, second := f.user(t, "First account"), f.user(t, "Second account")
	organization := uuid.NewV7()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO organizations(id,name,slug) VALUES($1,'Consent org','consent')`, organization); err != nil {
		t.Fatal(err)
	}
	f.member(t, organization, first, "member")
	f.member(t, organization, second, "member")
	original := f.session(t, first, organization)
	otherAccount := f.session(t, second, organization)
	otherSession := f.session(t, first, organization)
	var started api.DeviceStartResponse
	w := f.request(t, http.MethodPost, "/api/auth/device/start", "", "")
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &started) != nil {
		t.Fatal(w.Body.String())
	}
	var page api.DeviceStatusResponse
	w = f.request(t, http.MethodGet, "/api/auth/device/status?user_code="+url.QueryEscape(started.UserCode), original, "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Consent == nil || page.Consent.AccountName != "First account" || page.Consent.OrganizationName != "Consent org" {
		t.Fatalf("consent: %d %s", w.Code, w.Body)
	}
	body, _ := json.Marshal(api.DeviceAuthorizeRequest{UserCode: started.UserCode})
	proof := http.Header{"X-Helmr-Device-Consent": []string{page.Consent.Proof}}
	for _, token := range []string{otherAccount, otherSession} {
		for _, action := range []string{"approve", "deny"} {
			w = f.request(t, http.MethodPost, "/api/auth/device/"+action, token, string(body), proof)
			if w.Code != 409 || !strings.Contains(w.Body.String(), "device_identity_changed") {
				t.Fatalf("stale page admitted: %d %s", w.Code, w.Body)
			}
		}
	}
	w = f.request(t, http.MethodPost, "/api/auth/device/approve", original, string(body))
	if w.Code != 409 {
		t.Fatalf("missing proof admitted: %d", w.Code)
	}
	w = f.request(t, http.MethodPost, "/api/auth/device/approve", original, string(body), proof)
	if w.Code != 200 {
		t.Fatalf("original page denied: %d %s", w.Code, w.Body)
	}
}

func TestProviderConfirmationCannotBecomeAnonymousAfterRevokeAll(t *testing.T) {
	for _, mode := range []string{"bound", "anonymous", "stale-cookie"} {
		t.Run(mode, func(t *testing.T) {
			f := newHTTPPostgresFixture(t, func(cfg *ServerConfig) { cfg.AuthProvider = continuationAuthProvider{} })
			serve := func(path string, body any, token string, cookies []*http.Cookie) *httptest.ResponseRecorder {
				t.Helper()
				data, _ := json.Marshal(body)
				r := httptest.NewRequest(http.MethodPost, f.origin+path, bytes.NewReader(data))
				r.Header.Set("Origin", f.origin)
				if token != "" {
					r.AddCookie(&http.Cookie{Name: sessionCookieName(r), Value: token})
				}
				for _, cookie := range cookies {
					r.AddCookie(cookie)
				}
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, r)
				return w
			}
			start := func(token string) ([]*http.Cookie, string) {
				t.Helper()
				w := serve("/api/auth/github/start", api.GitHubAuthStartRequest{}, token, nil)
				var result api.GitHubAuthStartResponse
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
					t.Fatal(w.Code, w.Body)
				}
				u, err := url.Parse(result.RedirectURL)
				if err != nil {
					t.Fatal(err)
				}
				return w.Result().Cookies(), u.Query().Get("state")
			}
			finish := func(token string, cookies []*http.Cookie, state string) *httptest.ResponseRecorder {
				return serve("/api/auth/github/finish", api.GitHubAuthFinishRequest{State: state, Code: "fixture"}, token, cookies)
			}
			cookies, state := start("")
			initial := finish("", cookies, state)
			if initial.Code != 200 {
				t.Fatal(initial.Code, initial.Body)
			}
			token := ""
			for _, cookie := range initial.Result().Cookies() {
				if cookie.Name == sessionCookieName(httptest.NewRequest("GET", f.origin, nil)) {
					token = cookie.Value
				}
			}
			if token == "" {
				t.Fatal("missing login cookie")
			}
			callbackToken := token
			if mode == "anonymous" {
				callbackToken = ""
			}
			if mode != "stale-cookie" {
				cookies, state = start(callbackToken)
			}
			if revoked := serve("/api/account/access/revoke-all", map[string]any{}, token, nil); revoked.Code != 204 {
				t.Fatal(revoked.Code, revoked.Body)
			}
			if mode == "stale-cookie" {
				cookies, state = start(callbackToken)
			}
			completed := finish(callbackToken, cookies, state)
			if mode == "bound" {
				if completed.Code != 400 {
					t.Fatal("retired bound confirmation became a fresh login", completed.Code, completed.Body)
				}
				var active int
				if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM login_sessions WHERE revoked_at IS NULL`).Scan(&active); err != nil || active != 0 {
					t.Fatal(active, err)
				}
			} else if completed.Code != 200 {
				t.Fatal("anonymous sign-in denied", completed.Code, completed.Body)
			}
		})
	}
}
