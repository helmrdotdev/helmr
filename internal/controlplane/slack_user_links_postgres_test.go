package controlplane

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"github.com/golang-jwt/jwt/v5"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/jackc/pgx/v5"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/slack"
)

func TestSlackUserLinkHTTPProvesBothAccountsBeforeExplicitConfirmation(t *testing.T) {
	for _, scenario := range []string{"GET", "POST", "first-use"} {
		t.Run(scenario, func(t *testing.T) {
			callbackMethod := scenario
			if scenario == "first-use" {
				callbackMethod = "POST"
			}
			f := sessiontest.New(t)
			var org uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT org_id FROM environments WHERE id=$1`, f.Environment).Scan(&org); err != nil {
				t.Fatal(err)
			}
			installation, registration, publication := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_app_registrations(transport,id,org_id,app_id,created_by_user_id) VALUES('http',$1,$2,'app',$3)`, registration, org, f.User)
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,workspace_name,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id) VALUES($1,$6,$2,'app','team','bot','Workspace',1,$3,$4,'{chat:write}',$5)`, installation, org, bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{1}, 12), f.User, registration)
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_connections(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id,org_id) VALUES($1,$2,$3,$4,$5,$6,(SELECT org_id FROM slack_app_registrations WHERE id=$4))`, publication, f.Environment, f.Agent, registration, installation, f.User)
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
			nonce := ""
			slackActor := "human"
			exchanges := 0
			transport := slackRoundTripper(func(r *http.Request) (*http.Response, error) {
				var value any
				switch r.URL.Path {
				case "/api/openid.connect.token":
					exchanges++
					if err := r.ParseForm(); err != nil || r.PostForm.Get("redirect_uri") != "https://console.example.test/api/slack/user-links/callback" || r.PostForm.Get("code") != "private-code" {
						t.Fatal("unexpected token exchange")
					}
					token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": "https://slack.com", "aud": "client", "sub": slackActor, "https://slack.com/team_id": "team", "https://slack.com/user_id": slackActor, "name": "Slack Person", "nonce": nonce, "iat": time.Now().Add(-time.Second).Unix(), "exp": time.Now().Add(time.Minute).Unix()})
					token.Header["kid"] = "key"
					signed, err := token.SignedString(key)
					if err != nil {
						t.Fatal(err)
					}
					value = map[string]any{"ok": true, "id_token": signed, "access_token": "private-access"}
				case "/openid/connect/keys":
					value = map[string]any{"keys": []any{map[string]string{"kid": "key", "kty": "RSA", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}}
				default:
					t.Fatal(r.URL)
				}
				raw, _ := json.Marshal(value)
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
			})
			store, err := slack.NewCredentialStore(f.Pool, bytes.Repeat([]byte{1}, 32), slack.TransportHTTP, auditpolicy.Policy{RetentionSeconds: 3600})
			if err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET is_owner=true WHERE org_id=$1 AND user_id=$2`, org, f.User)
			f.SetPermissions(t, "sessions.create", "sessions.read", "sessions.send", "agent_connections.configure")
			if err = store.StoreAppCredentials(t.Context(), f.Principal(t), registration, slack.AppCredentials{ClientID: "client", ClientSecret: "private-secret", SigningSecret: "secret"}); err != nil {
				t.Fatal(err)
			}
			f.SetPermissions(t, "sessions.create", "deployments.publish", "deployments.promote", "sessions.read", "sessions.send", "computers.execute", "secrets.create", "api_keys.create")
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET app_registration_id=$2 WHERE id=$1`, installation, registration)
			cfg := completeServerConfig(t)
			cfg.AppOrigin = &url.URL{Scheme: "https", Host: "console.example.test"}
			queries := db.New(f.Pool)
			cfg.DB, cfg.TX, cfg.Auth = queries, f.Pool, identity.NewAPIKeyAuthenticator(f.Pool)
			cfg.Slack = &SlackConfig{ControlKey: bytes.Repeat([]byte{2}, 32), Credentials: store, Transport: transport, Client: slack.NewWebClient(store, nil)}
			handler, err := NewServer(cfg)
			if err != nil {
				t.Fatal(err)
			}
			keys, err := auth.NewKeys(cfg.AuthKey)
			if err != nil {
				t.Fatal(err)
			}
			fixture := httpPostgresFixture{pool: f.Pool, queries: queries, handler: handler, keys: keys, browserOrganizations: map[string]uuid.UUID{}, operatorTokens: map[string]bool{}, origin: cfg.AppOrigin.String()}
			session := fixture.session(t, f.User, org)
			other := fixture.user(t, "Other Person")
			fixture.member(t, org, other, "member")
			otherSession := fixture.session(t, other, org)
			call := func(method, path, token string, body any, cookies []*http.Cookie, want int) *httptest.ResponseRecorder {
				t.Helper()
				raw, _ := json.Marshal(body)
				r := httptest.NewRequest(method, "https://console.example.test"+path, bytes.NewReader(raw))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Origin", "https://console.example.test")
				if token != "" {
					r.AddCookie(&http.Cookie{Name: sessionCookieName(r), Value: token})
				}
				if id := fixture.browserOrganizations[token]; id != uuid.Nil() {
					r.Header.Set("X-Helmr-Organization", id.String())
				}
				// A browser replaces earlier Set-Cookie values for the same name.
				jar := map[string]*http.Cookie{}
				for _, cookie := range cookies {
					jar[cookie.Name] = cookie
				}
				for _, cookie := range jar {
					if cookie.MaxAge >= 0 {
						r.AddCookie(cookie)
					}
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != want {
					t.Fatalf("%s %s %d: %s", method, path, w.Code, w.Body.String())
				}
				if bytes.Contains(w.Body.Bytes(), []byte("private-")) {
					t.Fatal("token or code exposed")
				}
				return w
			}
			call("GET", "/api/slack/link-workspaces", session, nil, nil, 200)
			authorize := map[string]string{"installation_id": installation.String()}
			if scenario == "first-use" {
				channel, request := uuid.NewV7(), uuid.NewV7()
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_channel_bindings(id,environment_id,connection_id,installation_id,org_id,team_id,slack_channel_id) SELECT $1,$3,$4,$2,org_id,team_id,'C1' FROM slack_installations WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, channel, installation, f.Environment, publication)
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_requests(id,installation_id,request_key,request_digest,source_occurred_at,slack_user_id,payload_expired_at,status,expires_at,finished_at,error) VALUES($1,$2,'first-use',decode(repeat('11',32),'hex'),clock_timestamp(),'human',clock_timestamp(),'rejected',clock_timestamp()+interval '10 minutes',clock_timestamp(),'identity_unlinked')`, request, installation)
				payload := base64.RawURLEncoding.EncodeToString([]byte(request.String() + ":C1"))
				mac, err := auth.MAC(cfg.Slack.ControlKey, []byte("helmr.slack-link:"), []byte(payload))
				if err != nil {
					t.Fatal(err)
				}
				link := payload + "." + base64.RawURLEncoding.EncodeToString(mac)
				authorize = map[string]string{"link": link}
				call("GET", "/api/slack/user-links/first-use?link="+url.QueryEscape(link), "", nil, nil, 403)
				nonmember := fixture.user(t, "Nonmember")
				call("GET", "/api/slack/user-links/first-use?link="+url.QueryEscape(link), fixture.session(t, nonmember, uuid.Nil()), nil, nil, 403)
				// The link identifies the intended organization even before an org is selected.
				session = fixture.session(t, f.User, uuid.Nil())
				call("GET", "/api/slack/user-links/first-use?link="+url.QueryEscape(link), session, nil, nil, 200)
			}
			started := call("POST", "/api/slack/user-links/authorize", session, authorize, nil, 200)
			var redirect struct {
				URL string `json:"redirect_url"`
			}
			if err = json.Unmarshal(started.Body.Bytes(), &redirect); err != nil {
				t.Fatal(err)
			}
			target, err := url.Parse(redirect.URL)
			if err != nil {
				t.Fatal(err)
			}
			nonce = target.Query().Get("nonce")
			state := target.Query().Get("state")
			if nonce == "" || target.Query().Get("scope") != "openid profile" || target.Query().Get("response_mode") != "form_post" {
				t.Fatal("incorrect identity authorization")
			}
			// Both callback modes work without browser authentication cookies. Only a
			// fragment redirect results; verification and confirmation need authentication.
			form := url.Values{"state": {state}, "code": {"private-code"}}
			callbackURL := "https://console.example.test/api/slack/user-links/callback"
			callbackBody := ""
			if callbackMethod == "GET" {
				callbackURL += "?" + form.Encode()
			} else {
				callbackBody = form.Encode()
			}
			callback := httptest.NewRequest(callbackMethod, callbackURL, strings.NewReader(callbackBody))
			callback.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			bridge := httptest.NewRecorder()
			handler.ServeHTTP(bridge, callback)
			location, err := url.Parse(bridge.Header().Get("Location"))
			if err != nil || bridge.Code != 303 || location.Host != "console.example.test" || location.Path != "/auth/slack/link" || location.RawQuery != "" {
				t.Fatal("unsafe callback bridge", bridge.Code, err)
			}
			fragment, _ := url.ParseQuery(location.Fragment)
			if fragment.Get("code") != "private-code" || fragment.Get("state") != state || exchanges != 0 {
				t.Fatal("bridge exchanged or lost code")
			}
			input := map[string]string{"state": state, "code": "private-code"}
			call("POST", "/api/slack/user-links/verify", otherSession, input, started.Result().Cookies(), 400)
			if exchanges != 0 {
				t.Fatal("account switch exchanged code")
			}
			if scenario == "first-use" {
				slackActor = "different-human"
				call("POST", "/api/slack/user-links/verify", session, input, started.Result().Cookies(), 400)
				slackActor = "human"
			}
			// Confirming the sign-in mid-flow rotates the token but keeps the session.
			session = fixture.rotateSession(t, session)
			verified := call("POST", "/api/slack/user-links/verify", session, input, started.Result().Cookies(), 200)
			var proof struct {
				Confirmation string             `json:"confirmation"`
				ReturnURL    string             `json:"return_url"`
				Identity     slack.UserIdentity `json:"identity"`
			}
			if err = json.Unmarshal(verified.Body.Bytes(), &proof); err != nil || proof.Identity.SlackUserID != "human" || proof.Identity.TeamID != "team" || proof.Confirmation == "" {
				t.Fatal(err)
			}
			if scenario == "first-use" && proof.ReturnURL != "https://slack.com/app_redirect?channel=C1&team=team" {
				t.Fatal("missing Slack return destination")
			}
			var count int
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_user_links WHERE revoked_at IS NULL`).Scan(&count); err != nil || count != 0 {
				t.Fatal("link persisted before explicit confirmation", err)
			}
			call("POST", "/api/slack/user-links/confirm", session, map[string]any{"confirmation": proof.Confirmation, "confirmed": false}, verified.Result().Cookies(), 400)
			call("POST", "/api/slack/user-links/confirm", otherSession, map[string]any{"confirmation": proof.Confirmation, "confirmed": true}, verified.Result().Cookies(), 400)
			call("POST", "/api/slack/user-links/confirm", session, map[string]any{"confirmation": proof.Confirmation, "confirmed": true}, verified.Result().Cookies(), 204)
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_user_links WHERE user_id=$1 AND team_id='team' AND slack_user_id='human'`, f.User).Scan(&count); err != nil || count != 1 {
				t.Fatal("explicit link missing", err)
			}
			session = fixture.session(t, f.User, org)
			call("DELETE", "/api/slack/user-links/team/human", otherSession, nil, nil, 204)
			list := call("GET", "/api/slack/user-links", session, nil, nil, 200)
			if !strings.Contains(list.Body.String(), `"slack_user_id":"human"`) {
				t.Fatal("other user removed link")
			}
			call("DELETE", "/api/slack/user-links/team/human", session, nil, nil, 204)
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_user_links WHERE revoked_at IS NULL`).Scan(&count); err != nil || count != 0 {
				t.Fatal("unlink failed", err)
			}
			call("POST", "/api/auth/logout", session, nil, nil, 204)
			freshSession := fixture.session(t, f.User, org)
			previousExchanges := exchanges
			call("POST", "/api/slack/user-links/verify", freshSession, input, started.Result().Cookies(), 400)
			if exchanges != previousExchanges {
				t.Fatal("old session flow exchanged provider code")
			}

		})
	}
}
