package slack

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"strings"
	"testing"
	"uuid"
)

func TestExplicitSlackStartChecksDestination(t *testing.T) {
	for _, mode := range []string{"valid", "target-denied", "target-unpublished"} {
		t.Run(mode, func(t *testing.T) {
			f := newSlackAdmissionFixture(t)
			target, registration, installation, publication := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'target');
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy) SELECT environment_id,$2,deployment_id,template_id,has_init,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3;
 INSERT INTO slack_app_registrations(transport,id,org_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id) SELECT 'http',$4,org_id,'target-app','target-client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$7 FROM environments WHERE id=$1;
 INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id) SELECT $5,$4,org_id,'target-app','team','target-bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['app_mentions:read','channels:read','channels:history','groups:read','groups:history','chat:write','assistant:write'],$7 FROM environments WHERE id=$1;
 INSERT INTO slack_connections(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id,org_id) VALUES($6,$1,$2,$4,$5,$7,(SELECT org_id FROM slack_app_registrations WHERE id=$4))`, pgx.QueryExecModeSimpleProtocol, f.Environment, target, f.Agent, registration, installation, publication, f.User)
			if mode == "target-unpublished" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_connections SET revoked_at=clock_timestamp() WHERE id=$1`, publication)
			}
			calls := 0
			client := NewWebClient(credentialsFunc(func(_ context.Context, id uuid.UUID, revision int64) (string, error) {
				if id != installation || revision != 1 {
					t.Fatal("consulted source transport", id)
				}
				return "target-token", nil
			}), roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/api/conversations.info" || r.URL.Query().Get("channel") != "C1" {
					t.Fatal(r.URL)
				}
				raw, _ := json.Marshal(map[string]any{"ok": true, "channel": map[string]any{"id": "C1", "name": "work", "context_team_id": "team", "is_channel": true, "is_member": mode != "target-denied"}})
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
			}))
			f.SetPermissions(t, "sessions.create")
			caller := session.UserCaller(f.Principal(t))
			channel := "C1"
			result, err := Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, client, caller, session.StartRequest{EnvironmentID: f.Environment, Agent: "target", SlackChannelID: &channel, IdempotencyKey: "explicit-target", Input: json.RawMessage(`[{"type":"text","text":"private input"}]`)})
			if mode == "target-denied" || mode == "target-unpublished" {
				want := slackroute.ErrChannelUnavailable
				if mode == "target-unpublished" {
					want = slackroute.ErrTargetNotPublished
				}
				if !errors.Is(err, want) {
					t.Fatal(err)
				}
				return
			}
			if err != nil || calls != 1 {
				t.Fatal(calls, err)
			}
			var pinned bool
			var payload []byte
			if err = f.Pool.QueryRow(t.Context(), `SELECT s.deployment_id=$2,p.payload FROM sessions s JOIN slack_posts p ON p.session_id=s.id WHERE s.id=$1 AND p.role='opening'`, result.SessionID, f.Deployment).Scan(&pinned, &payload); err != nil || !pinned {
				t.Fatal(pinned, err)
			}
			if strings.Contains(string(payload), "private input") {
				t.Fatal("input implicitly disclosed")
			}
			var sender uuid.UUID
			if err = f.Pool.QueryRow(t.Context(), `SELECT c.installation_id FROM slack_threads t JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id WHERE t.front_session_id=$1`, result.SessionID).Scan(&sender); err != nil || sender != installation {
				t.Fatal(sender, err)
			}
		})
	}
}
