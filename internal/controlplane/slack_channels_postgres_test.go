package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/slack"
	"github.com/jackc/pgx/v5"
)

type slackHTTPTestCredentials struct{}

func (slackHTTPTestCredentials) BotToken(context.Context, uuid.UUID, int64) (string, error) {
	return "test-token", nil
}

func TestSlackPublishedStartHTTPAndSDK(t *testing.T) {
	f := agenttest.New(t)
	installation, registration, publication := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	channel := "C1"
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparation_specs SET seed=jsonb_build_object('profile',$7::text) WHERE environment_id=$5;
 UPDATE computer_definitions SET resources='{"milliCpu":1000,"memoryMiB":512}' WHERE environment_id=$5;
 UPDATE org_members SET role='owner' WHERE user_id=$4;
 INSERT INTO slack_app_registrations(id,organization_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id)
 SELECT $2,org_id,'app','client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$4 FROM environments WHERE id=$5;
 INSERT INTO slack_installations(id,app_registration_id,organization_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id)
 SELECT $1,$2,org_id,'app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['app_mentions:read','channels:read','channels:history','groups:read','groups:history','chat:write','assistant:write'],$4 FROM environments WHERE id=$5;
 INSERT INTO agent_publications(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id) VALUES($3,$5,$6,$2,$1,$4);`, pgx.QueryExecModeSimpleProtocol, installation, registration, publication, f.User, f.Environment, f.Agent, definition.ComputerSeedProfile)
	var orgID, projectID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&orgID, &projectID); err != nil {
		t.Fatal(err)
	}
	queries := db.New(f.Pool)
	cfg := completeServerConfig(t)
	cfg.PublicURL = &url.URL{Scheme: "https", Host: "console.example.test"}
	cfg.DB, cfg.TX, cfg.Auth = queries, f.Pool, identity.NewAPIKeyAuthenticator(queries)
	store, err := slack.NewCredentialStore(f.Pool, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	preflights := 0
	cfg.Slack = &SlackConfig{Credentials: store, ControlKey: bytes.Repeat([]byte{2}, 32), Client: slack.NewWebClient(slackHTTPTestCredentials{}, slackRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/conversations.info" || r.URL.Query().Get("channel") != channel {
			t.Fatalf("unexpected Slack request: %s %s", r.Method, r.URL)
		}
		preflights++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"channel":{"id":"C1","name":"work","context_team_id":"team","is_channel":true,"is_member":true}}`)), Header: http.Header{}}, nil
	}))}
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewKeys(cfg.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	h := httpPostgresFixture{pool: f.Pool, queries: queries, handler: handler, keys: keys}
	owner := h.session(t, f.User, orgID)
	base := fmt.Sprintf("/api/projects/%s/environments/%s", projectID, f.Environment)
	call := func(method, path, token, body string, status int) []byte {
		t.Helper()
		r := h.request(t, method, path, token, body)
		if r.Code != status {
			t.Fatalf("%s %s: %d %s want %d", method, path, r.Code, r.Body.String(), status)
		}
		return r.Body.Bytes()
	}
	expectCode := func(raw []byte, code string) {
		t.Helper()
		var reply api.HTTPErrorResponse
		if err := json.Unmarshal(raw, &reply); err != nil || reply.Error.Code != code {
			t.Fatalf("code %s want %s: %v", raw, code, err)
		}
	}
	call("GET", base+"/slack/channels", owner, "", 404)
	call("GET", base+"/slack/channels/"+channel, owner, "", 404)
	for _, slack := range []string{`null`, `{}`, `{"channel_id":""}`, `{"channel_id":"bad"}`, fmt.Sprintf(`{"channel_id":%q,"other":true}`, channel), `[]`} {
		call("POST", base+"/agents/agent/start", owner, `{"input":[],"slack":`+slack+`}`, 400)
	}
	viewerID := h.user(t, "Viewer")
	h.member(t, orgID, viewerID, db.OrgMemberRoleViewer)
	viewer := h.session(t, viewerID, orgID)
	call("GET", base+"/slack/channels", viewer, "", 404)
	call("GET", base+"/slack/channels/"+channel, viewer, "", 404)
	call("POST", base+"/agents/agent/start", viewer, fmt.Sprintf(`{"input":[],"slack":{"channel_id":%q}}`, channel), 404)
	issuer := auth.Principal{OrgID: orgID, UserID: f.User, Kind: auth.PrincipalKindSession, Role: auth.RoleOwner}
	scope := auth.Scope{OrgID: orgID, ProjectID: projectID.String(), EnvironmentID: f.Environment.String()}
	key, err := identity.IssueAPIKey(t.Context(), queries, issuer, scope, identity.APIKeyInput{Name: "Slack SDK", Permissions: []auth.Permission{auth.PermissionAgentsStart, auth.PermissionSessionsRead, auth.PermissionSessionsSend}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	script, err := filepath.Abs("../../sdk/typescript/testdata/slack-http.ts")
	if err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]string{"url": server.URL, "apiKey": key.Raw, "channelId": channel})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "bun", script)
	command.Stdin = bytes.NewReader(config)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("SDK HTTP: %v\n%s", err, output)
	}
	call("POST", base+"/agents/agent/start", owner, fmt.Sprintf(`{"input":[],"session_key":"http-route","idempotency_key":"http-first","slack":{"channel_id":%q}}`, channel), 200)
	missing := "COTHER"
	expectCode(call("POST", base+"/agents/agent/start", owner, fmt.Sprintf(`{"input":[],"session_key":"http-route","idempotency_key":"http-first","slack":{"channel_id":%q}}`, missing), 409), "idempotency_conflict")
	expectCode(call("POST", base+"/agents/agent/start", owner, fmt.Sprintf(`{"input":[],"session_key":"http-route","idempotency_key":"http-next","slack":{"channel_id":%q}}`, missing), 409), "slack_destination_conflict")
	before := preflights
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_publications SET revoked_at=clock_timestamp() WHERE id=$1`, publication)
	call(http.MethodGet, "/v1/slack/channels/"+channel, key.Raw, "", 404)
	call("POST", base+"/agents/agent/start", owner, fmt.Sprintf(`{"input":[],"session_key":"http-route","idempotency_key":"http-first","slack":{"channel_id":%q}}`, channel), 200)
	call("POST", base+"/agents/agent/start", owner, `{"input":[],"session_key":"http-route","idempotency_key":"http-continue"}`, 200)
	expectCode(call(http.MethodPost, "/v1/agents/agent/start", key.Raw, fmt.Sprintf(`{"input":[],"slack":{"channel_id":%q}}`, channel), 409), "target_not_published")
	if preflights != before {
		t.Fatal("retired publication triggered another preflight")
	}
}
