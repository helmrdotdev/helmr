package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestAuthContinuationUsesEffectiveOrganization(t *testing.T) {
	fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		cfg.DeploymentMode = deploymentModeManagedCloud
	})
	user := fixture.user(t, "Developer")
	orgA, orgB := uuid.NewV7(), uuid.NewV7()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := fixture.pool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO organizations (id, name, slug) VALUES ($1, 'First org', 'first'), ($2, 'Invited org', 'invited')", orgA.String(), orgB.String())
	exec("INSERT INTO org_members (org_id, user_id, role, created_at) VALUES ($1, $3, 'owner', now() - interval '1 day'), ($2, $3, 'viewer', now())", orgA.String(), orgB.String(), user.String())
	exec("INSERT INTO regions (id, display_name) VALUES ('local', 'Local')")
	exec("INSERT INTO projects (id, org_id, default_region_id, slug, name) VALUES ($1, $2, 'local', 'first', 'First')", uuid.NewV7().String(), orgA.String())
	me := func(token string) (int, api.MeResponse) {
		t.Helper()
		w := fixture.request(t, http.MethodGet, "/api/me", token, "")
		var response api.MeResponse
		if w.Code == http.StatusOK && json.Unmarshal(w.Body.Bytes(), &response) != nil {
			t.Fatalf("me: %s", w.Body.String())
		}
		return w.Code, response
	}

	pinned := fixture.session(t, user, orgB)
	if status, response := me(pinned); status != http.StatusOK || response.OrgID != orgB.String() || response.OrgName != "Invited org" ||
		response.Role != "viewer" || !response.ProjectRequired || response.OrganizationRequired {
		t.Fatalf("me used another membership: %d %+v", status, response)
	} else {
		for _, permission := range response.Permissions {
			if permission == "tasks.deploy" {
				t.Fatal("advertised first org owner permission")
			}
		}
	}

	// Ordinary unpinned login retains the existing first-membership selection.
	unpinned := fixture.session(t, user, uuid.Nil())
	if status, response := me(unpinned); status != http.StatusOK || response.OrgID != orgA.String() || response.Role != "owner" || response.ProjectRequired {
		t.Fatalf("ordinary login: %d %+v", status, response)
	}

	start := fixture.request(t, http.MethodPost, "/api/auth/device/start", "", "")
	var device api.DeviceStartResponse
	if start.Code != http.StatusCreated || json.Unmarshal(start.Body.Bytes(), &device) != nil {
		t.Fatalf("start: %s", start.Body.String())
	}
	if device.VerificationURIComplete != "https://console.example.test/auth/device?code="+url.QueryEscape(device.UserCode) {
		t.Fatalf("verification URI = %q", device.VerificationURIComplete)
	}
	exchange := func() *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(api.DeviceTokenRequest{DeviceCode: device.DeviceCode})
		return fixture.request(t, http.MethodPost, "/api/auth/device/token", "", string(body))
	}
	if w := exchange(); w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "authorization_pending") {
		t.Fatalf("pending exchange: %d %s", w.Code, w.Body.String())
	}
	status := fixture.request(t, http.MethodGet, "/api/auth/device/status?user_code="+url.QueryEscape(device.UserCode), pinned, "")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"status":"pending"`) {
		t.Fatalf("status: %d %s", status.Code, status.Body.String())
	}
	consent := func(userID, orgID string) string {
		b, _ := json.Marshal(api.DeviceAuthorizeRequest{UserCode: device.UserCode, UserID: userID, OrgID: orgID})
		return string(b)
	}
	// Another tab changed org or user: the displayed context must not silently bind.
	for _, body := range []string{consent(user.String(), orgA.String()), consent(uuid.NewV7().String(), orgB.String()), consent("", "")} {
		w := fixture.request(t, http.MethodPost, "/api/auth/device/approve", pinned, body)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "device_identity_changed") {
			t.Fatalf("mismatch: %d %s", w.Code, w.Body.String())
		}
	}
	if w := fixture.request(t, http.MethodPost, "/api/auth/device/approve", pinned, consent(user.String(), orgB.String())); w.Code != http.StatusOK {
		t.Fatalf("org without project could not approve: %d %s", w.Code, w.Body.String())
	}
	hash, _ := auth.HashToken(fixture.keys.DeviceCode, auth.NormalizeUserCode(device.UserCode))
	decided, err := fixture.queries.GetDeviceCodeByUserCodeHash(t.Context(), hash)
	if err != nil || decided.OrgID != pgvalue.UUID(orgB) || decided.Status != db.DeviceCodeStatusApproved {
		t.Fatalf("approval: %+v %v", decided, err)
	}
	w := exchange()
	var token api.DeviceTokenResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &token) != nil || token.TokenType != "bearer" || token.ExpiresInSeconds != int64((30*24*time.Hour).Seconds()) {
		t.Fatalf("exchange: %d %s", w.Code, w.Body.String())
	}
	if status, response := me(token.AccessToken); status != http.StatusOK || response.OrgID != orgB.String() {
		t.Fatalf("device session: %d %+v", status, response)
	}
	if w := exchange(); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_request") {
		t.Fatalf("second exchange: %d %s", w.Code, w.Body.String())
	}

	// Missing membership is onboarding, not authorization. Revoked pinned membership
	// instead invalidates that session: it cannot fall through to another org.
	newcomer := fixture.user(t, "New user")
	newToken := fixture.session(t, newcomer, uuid.Nil())
	if status, response := me(newToken); status != http.StatusOK || !response.OrganizationRequired || response.OrgID != "" {
		t.Fatalf("newcomer: %d %+v", status, response)
	}
	if w := fixture.request(t, http.MethodPost, "/api/auth/device/approve", newToken, consent(newcomer.String(), "")); w.Code != http.StatusForbidden {
		t.Fatalf("no org approved: %d", w.Code)
	}
	exec("UPDATE org_members SET disabled_at = now() WHERE org_id = $1 AND user_id = $2", orgB.String(), user.String())
	if status, _ := me(pinned); status != http.StatusUnauthorized {
		t.Fatalf("revoked pinned org accepted: %d", status)
	}
	if status, _ := me(unpinned); status != http.StatusOK {
		t.Fatalf("active first membership lost: %d", status)
	}

	if w := fixture.request(t, http.MethodPost, "/api/auth/logout", unpinned, ""); w.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
	if status, _ := me(unpinned); status != http.StatusUnauthorized {
		t.Fatalf("logged out session accepted: %d", status)
	}
}

func TestAuthContinuationProvidersReturnStoredDestination(t *testing.T) {
	fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		cfg.PublicURL = &url.URL{Scheme: "https", Host: "helmr.example.test"}
		cfg.AuthProvider = continuationAuthProvider{}
	})
	const next = "/auth/device?code=ABCD-EFGH#confirm"
	serve := func(r *http.Request) *httptest.ResponseRecorder {
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

	token := "magic-link-fixture-token"
	hash, err := auth.HashToken(fixture.keys.MagicLink, token)
	if err != nil {
		t.Fatal(err)
	}
	link, err := fixture.queries.CreateMagicLink(t.Context(), db.CreateMagicLinkParams{ID: pgvalue.UUID(uuid.NewV7()), Purpose: db.MagicLinkPurposeLogin, TokenHash: hash, Email: "fixture@example.test", RedirectAfter: pgtype.Text{String: next, Valid: true}, ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.MarkMagicLinkSent(t.Context(), link.ID); err != nil {
		t.Fatal(err)
	}
	payload, _ = json.Marshal(api.MagicLinkFinishRequest{Token: token})
	r = httptest.NewRequest(http.MethodPost, "https://helmr.example.test/api/auth/magic-link/finish", bytes.NewReader(payload))
	finish = serve(r)
	if finish.Code != http.StatusOK || json.Unmarshal(finish.Body.Bytes(), &finished) != nil || finished.RedirectAfter != next || !hasSessionCookie(finish, r) {
		t.Fatalf("magic link lost next: %d %s", finish.Code, finish.Body.String())
	}
	payload, _ = json.Marshal(api.MagicLinkFinishRequest{Token: token})
	finish = serve(httptest.NewRequest(http.MethodPost, "https://helmr.example.test/api/auth/magic-link/finish", bytes.NewReader(payload)))
	if finish.Code != http.StatusBadRequest || !strings.Contains(finish.Body.String(), `"code":"invalid_token"`) {
		t.Fatalf("reused magic link: %d %s", finish.Code, finish.Body.String())
	}
}

func hasSessionCookie(w *httptest.ResponseRecorder, r *http.Request) bool {
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == sessionCookieName(r) && cookie.Value != "" && cookie.MaxAge == int((30*24*time.Hour).Seconds()) {
			return true
		}
	}
	return false
}
