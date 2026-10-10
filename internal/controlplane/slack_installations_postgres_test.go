package controlplane

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/slack"
)

type slackRoundTripper func(*http.Request) (*http.Response, error)

func (f slackRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSlackInstallationHTTPBindsBrowserIdentityAndKeepsCredentialsPrivate(t *testing.T) {
	f := agenttest.New(t)
	var org uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id FROM environments WHERE id=$1`, f.Environment).Scan(&org); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='owner' WHERE org_id=$1 AND user_id=$2`, org, f.User)
	store, err := slack.NewCredentialStore(f.Pool, bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	exchanges := 0
	transport := slackRoundTripper(func(r *http.Request) (*http.Response, error) {
		exchanges++
		if err := r.ParseForm(); err != nil || r.Form.Get("redirect_uri") != "https://console.example.test/auth/slack/callback" || r.Form.Get("code") != "verified-code" {
			t.Fatal("wrong OAuth exchange")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"app_id":"app","team":{"id":"team","name":"Workspace"},"bot_user_id":"bot","token_type":"bot","access_token":"private-access","scope":"app_mentions:read,channels:read,channels:history,groups:read,groups:history,chat:write,assistant:write"}`))}, nil
	})
	cfg := completeServerConfig(t)
	cfg.PublicURL = &url.URL{Scheme: "https", Host: "console.example.test"}
	queries := db.New(f.Pool)
	cfg.DB, cfg.TX, cfg.Auth = queries, f.Pool, identity.NewAPIKeyAuthenticator(queries)
	cfg.Slack = &SlackConfig{ControlKey: bytes.Repeat([]byte{4}, 32), Credentials: store, Transport: transport, Client: slack.NewWebClient(store, nil)}
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewKeys(cfg.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	h := httpPostgresFixture{pool: f.Pool, queries: queries, handler: handler, keys: keys}
	owner := h.session(t, f.User, org)
	call := func(method, path, token string, body any, cookies []*http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "https://console.example.test"+path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if bytes.Contains(w.Body.Bytes(), []byte("private-")) || bytes.Contains(w.Body.Bytes(), []byte("ciphertext")) {
			t.Fatal("credential exposed in HTTP")
		}
		return w
	}
	var project uuid.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT project_id FROM environments WHERE id=$1`, f.Environment).Scan(&project); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.Environment, f.Deployment)
	base := "/api/projects/" + project.String() + "/environments/" + f.Environment.String() + "/agents/agent/slack"
	created := call("POST", base, owner, nil, nil)
	var result struct {
		Publication slack.PublicationView `json:"publication"`
	}
	if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &result) != nil {
		t.Fatal("setup failed", created.Code, created.Body.String())
	}
	publication := result.Publication
	if !bytes.Contains(created.Body.Bytes(), []byte("/integrations/slack/apps/"+publication.RegistrationID.String()+"/events")) {
		t.Fatal("manifest lacks exact registration")
	}
	if w := call("PUT", base+"/"+publication.ID.String()+"/credentials", owner, slack.AppCredentials{ClientID: "client", ClientSecret: "private-client", SigningSecret: "signing-secret"}, nil); w.Code != 204 {
		t.Fatal("credentials rejected", w.Code, w.Body.String())
	}
	start := func() (string, []*http.Cookie) {
		t.Helper()
		w := call("POST", base+"/"+publication.ID.String()+"/authorize", owner, nil, nil)
		var response struct {
			URL string `json:"redirect_url"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
			t.Fatalf("start: %d %s", w.Code, w.Body.String())
		}
		redirect, err := url.Parse(response.URL)
		if err != nil || redirect.Host != "slack.com" || redirect.Query().Get("client_id") != "client" || redirect.Query().Get("scope") == "" {
			t.Fatal("wrong authorization URL")
		}
		return redirect.Query().Get("state"), w.Result().Cookies()
	}
	finish := func(state, token string, cookies []*http.Cookie) *httptest.ResponseRecorder {
		return call("POST", "/api/slack/installations/finish", token, map[string]string{"state": state, "code": "verified-code"}, cookies)
	}
	if w := call("GET", base, owner, nil, nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	state, cookies := start()
	if w := finish("wrong-state", owner, cookies); w.Code != 400 || exchanges != 0 {
		t.Fatal("wrong state exchanged code")
	}
	other := h.user(t, "Other admin")
	h.member(t, org, other, db.OrgMemberRoleAdmin)
	otherToken := h.session(t, other, org)
	if w := finish(state, otherToken, cookies); w.Code != 400 || exchanges != 0 {
		t.Fatal("switched user exchanged code")
	}
	if w := call("POST", "/api/auth/github/finish", owner, map[string]string{"state": state, "code": "verified-code"}, cookies); w.Code == 200 || exchanges != 0 {
		t.Fatal("Slack flow accepted as GitHub flow")
	}
	w := finish(state, owner, cookies)
	var connected struct {
		ID string `json:"installation_id"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &connected) != nil || connected.ID == "" || exchanges != 1 {
		t.Fatalf("finish: %d %s", w.Code, w.Body.String())
	}
	installation, err := ids.Parse(connected.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token, err := store.BotToken(t.Context(), installation, 1); err != nil || token != "private-access" {
		t.Fatal("encrypted custody unavailable", err)
	}
	state, cookies = start()
	w = finish(state, owner, cookies)
	var reauthorized struct {
		ID string `json:"installation_id"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &reauthorized) != nil || reauthorized.ID != connected.ID || exchanges != 2 {
		t.Fatal("reauthorization changed generation")
	}
	if w := call("GET", base, owner, nil, nil); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"status":"connected"`)) {
		t.Fatal("missing connection state", w.Body.String())
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='developer' WHERE org_id=$1 AND user_id=$2`, org, f.User)
	if w := call("DELETE", base+"/"+publication.ID.String(), owner, nil, nil); w.Code != 403 {
		t.Fatal("non-manager disconnected")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='owner' WHERE org_id=$1 AND user_id=$2`, org, f.User)
	if w := call("DELETE", base+"/"+publication.ID.String(), owner, nil, nil); w.Code != 204 {
		t.Fatal("disconnect failed", w.Code, w.Body.String())
	}
	if _, err := store.BotToken(t.Context(), installation, 2); err == nil {
		t.Fatal("disconnected credentials decrypted")
	}
	if w := call("POST", base+"/"+publication.ID.String()+"/authorize", owner, nil, nil); w.Code != 409 {
		t.Fatal("retired connection offered reauthorization")
	}
	// Mounted transport handlers verify the exact raw body before accepting work.
	body := []byte(`{"type":"url_verification","api_app_id":"app","challenge":"challenge"}`)
	r := httptest.NewRequest("POST", "https://console.example.test/integrations/slack/apps/"+publication.RegistrationID.String()+"/events", bytes.NewReader(body))
	unsigned := httptest.NewRecorder()
	handler.ServeHTTP(unsigned, r)
	if unsigned.Code != 401 {
		t.Fatal("unsigned event accepted")
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("signing-secret"))
	_, _ = mac.Write(append([]byte("v0:"+timestamp+":"), body...))
	r = httptest.NewRequest("POST", "https://console.example.test/integrations/slack/apps/"+publication.RegistrationID.String()+"/events", bytes.NewReader(body))
	r.Header.Set("X-Slack-Request-Timestamp", timestamp)
	r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	signed := httptest.NewRecorder()
	handler.ServeHTTP(signed, r)
	if signed.Code != 401 {
		t.Fatal("retired registration accepted signed event")
	}
}
