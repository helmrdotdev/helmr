package controlplane

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/slack"
	"github.com/jackc/pgx/v5"
)

func TestSlackDeliveryHTTPReadsCurrentScopeAndRestrictsRecoveryToManagers(t *testing.T) {
	f := agenttest.New(t)
	var org, project uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	installation, channel, publication, thread, participant, registration := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='owner' WHERE user_id=$1;
 INSERT INTO slack_app_registrations(id,organization_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id) VALUES($11,$3,'app','client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$1);
 INSERT INTO slack_installations(id,app_registration_id,organization_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id) VALUES($2,$11,$3,'app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['app_mentions:read','channels:read','channels:history','groups:read','groups:history','chat:write','assistant:write'],$1);
 INSERT INTO agent_publications(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id) VALUES($6,$5,$7,$11,$2,$1);
 INSERT INTO slack_channels(id,environment_id,publication_id,installation_id,organization_id,team_id,slack_channel_id) VALUES($4,$5,$6,$2,$3,'team','C1');
 UPDATE sessions SET slack_channel_id=$4 WHERE id=$8;
 INSERT INTO slack_threads(id,environment_id,front_session_id,channel_id,organization_id,team_id,slack_channel_id,thread_ts,opening_publication_key) VALUES($9,$5,$8,$4,$3,'team','C1','123.456','opening');
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($10,$9,$5,$8)`, pgx.QueryExecModeSimpleProtocol, f.User, installation, org, channel, f.Environment, publication, f.Agent, f.Session, thread, participant, registration)
	var first uuid.UUID
	var attempt uuid.UUID
	for seq := 1; seq <= 2; seq++ {
		post := uuid.NewV7()
		body := []byte(`{"text":"visible fallback","blocks":[{"type":"actions","elements":[{"value":"DO_NOT_RETURN_SIGNED_CONTROL"}]}]}`)
		digest := sha256.Sum256(body)
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_posts(id,environment_id,session_id,thread_id,thread_source_id,seq,publication_key,role,payload,payload_digest,presentation_path) VALUES($1,$2,$3,(SELECT thread_id FROM slack_thread_sources WHERE id=$4),$4,$5,$6,'lifecycle',$7,$8,'post'); UPDATE slack_installations SET delivery_next_at=clock_timestamp() WHERE id=$9`, pgx.QueryExecModeSimpleProtocol, post, f.Environment, f.Session, participant, seq, fmt.Sprint(seq), body, digest[:], installation)
		claim, err := slack.ClaimPost(t.Context(), f.Pool, post)
		if err != nil || claim == nil {
			t.Fatal(claim, err)
		}
		if err = slack.FinishPost(t.Context(), f.Pool, *claim, slack.DeliveryResult{Disposition: slack.Uncertain}); err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET reconciliation_paused_at=clock_timestamp() WHERE id=$1`, post)
		if seq == 1 {
			first, attempt = post, claim.AttemptID
		}
	}
	queries := db.New(f.Pool)
	cfg := completeServerConfig(t)
	cfg.PublicURL = &url.URL{Scheme: "https", Host: "console.example.test"}
	cfg.DB, cfg.TX, cfg.Auth = queries, f.Pool, identity.NewAPIKeyAuthenticator(queries)
	store, err := slack.NewCredentialStore(f.Pool, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Slack = &SlackConfig{ControlKey: bytes.Repeat([]byte{2}, 32), Credentials: store, Client: slack.NewWebClient(store, nil)}
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
	base := fmt.Sprintf("/api/projects/%s/environments/%s/sessions/%s/slack-delivery", project, f.Environment, f.Session)
	call := func(method, path, token, body string, want int) []byte {
		t.Helper()
		w := h.request(t, method, path, token, body)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	raw := call(http.MethodGet, base+"?limit=1", owner, "", 200)
	var page struct {
		Posts []slack.DeliveryView `json:"posts"`
		Next  string               `json:"next_cursor"`
	}
	if err = json.Unmarshal(raw, &page); err != nil || len(page.Posts) != 1 || page.Next != "1" || page.Posts[0].ID != first || page.Posts[0].Text != "visible fallback" {
		t.Fatal(string(raw), err)
	}
	if bytes.Contains(raw, []byte("DO_NOT_RETURN")) || bytes.Contains(raw, []byte("inflight_payload")) {
		t.Fatal("frozen controls escaped")
	}
	call(http.MethodGet, base+"?cursor=1&limit=1", owner, "", 200)
	call(http.MethodGet, base+"?cursor=-1", owner, "", 400)
	input, _ := json.Marshal(slack.DeliveryRecovery{AttemptID: attempt, PublicationRevision: 1})
	action := base + "/" + first.String()
	call(http.MethodPost, action+"/check", owner, string(input), 204)
	call(http.MethodPost, action+"/check", owner, string(input), 409)
	call(http.MethodGet, fmt.Sprintf("/api/projects/%s/environments/%s/sessions/%s/slack-delivery", project, f.Environment, uuid.NewV7()), owner, "", 404)
	viewerID := h.user(t, "Viewer")
	h.member(t, org, viewerID, db.OrgMemberRoleViewer)
	viewer := h.session(t, viewerID, org)
	call(http.MethodGet, base, viewer, "", 200)
	call(http.MethodPost, action+"/abandon", viewer, string(input), 403)
	issuer := auth.Principal{OrgID: org, UserID: f.User, Kind: auth.PrincipalKindSession, Role: auth.RoleOwner}
	key, err := identity.IssueAPIKey(t.Context(), queries, issuer, auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: f.Environment.String()}, identity.APIKeyInput{Name: "delivery read", Permissions: []auth.Permission{auth.PermissionSessionsRead}})
	if err != nil {
		t.Fatal(err)
	}
	call(http.MethodPost, action+"/abandon", key.Raw, string(input), 401)
	call(http.MethodPost, action+"/abandon", owner, string(input), 204)
	call(http.MethodPost, action+"/abandon", owner, string(input), 204)
	raw = call(http.MethodGet, base, owner, "", 200)
	if !bytes.Contains(raw, []byte(`"disposed_by":"`+f.User.String()+`"`)) || !bytes.Contains(raw, []byte(`"status":"failed"`)) {
		t.Fatal("disposition not visible", string(raw))
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='developer' WHERE user_id=$1`, f.User)
	call(http.MethodPost, action+"/check", owner, string(input), 403)
}
