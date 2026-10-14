package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/slack"
	"github.com/jackc/pgx/v5"
)

// attentionSession adds an open Session of the fixture Agent and returns it.
func attentionSession(t *testing.T, f sessiontest.Fixture) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth) VALUES($1,$2,$3,$4,$5,$2,0)`, f.Environment, id, f.Agent, f.Deployment, f.Computer)
	return id
}

// attentionFailedTurn admits a Turn in the Session and records it failed hoursAgo.
func attentionFailedTurn(t *testing.T, f sessiontest.Fixture, sessionID uuid.UUID, key string, hoursAgo int) uuid.UUID {
	t.Helper()
	admission, err := session.Enqueue(t.Context(), f.Pool, nil, session.Caller{Kind: "user", ID: f.User}, session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: sessionID, IdempotencyKey: key, Input: json.RawMessage(`[{"type":"text","text":"work"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE turns SET status='failed',error_code='agent_error',error_message='\x6f6f7073',terminal_at=clock_timestamp()-make_interval(hours=>$2) WHERE id=$1`, admission.TurnID, hoursAgo)
	return admission.TurnID
}

func TestAttentionHTTPListsWaitingWorkOldestFirstUnderReadAuthority(t *testing.T) {
	f := sessiontest.New(t)
	var org, project uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	caller := session.Caller{Kind: "user", ID: f.User}

	// A pending Question, newest of all.
	e := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	asked, err := session.Enqueue(t.Context(), f.Pool, nil, caller, session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "ask", Input: json.RawMessage(`[{"type":"text","text":"work"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchRuntimeTurn(t, f.Pool, e); err != nil {
		t.Fatal(err)
	}
	question := uuid.NewV7()
	if err := session.RuntimeAsk(t.Context(), f.Pool, fixtureHostFor(t, f.Pool, e), e, asked.TurnID, question, json.RawMessage(`{"prompt":[{"type":"json","value":1},{"type":"text","text":"Ship\n the  release?\u0000"}],"answer":{"type":"text"}}`)); err != nil {
		t.Fatal(err)
	}
	answered := uuid.NewV7()
	if err := session.RuntimeAsk(t.Context(), f.Pool, fixtureHostFor(t, f.Pool, e), e, asked.TurnID, answered, json.RawMessage(`{"prompt":[{"type":"text","text":"Already answered"}],"answer":{"type":"text"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := session.RespondAsk(t.Context(), f.Pool, caller, session.AskAnswerRequest{EnvironmentID: f.Environment, SessionID: f.Session, TurnID: asked.TurnID, AskID: answered, ResponseID: "reply", Answer: json.RawMessage(`"yes"`)}); err != nil {
		t.Fatal(err)
	}

	// The oldest item: a hold on an open Session. Released holds and holds on
	// Sessions that are no longer open do not wait on anyone.
	held, closed := attentionSession(t, f), attentionSession(t, f)
	hold := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason,created_at) VALUES($1,$2,$3,'local','Approve the deploy',clock_timestamp()-interval '3 hours');
 INSERT INTO session_holds(environment_id,id,session_id,scope,reason,released_at) VALUES($1,$4,$3,'local','released',clock_timestamp());
 INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$5,$6,'local','closed');
 UPDATE sessions SET status='closed' WHERE environment_id=$1 AND id=$6`, pgx.QueryExecModeSimpleProtocol, f.Environment, hold, held, uuid.NewV7(), uuid.NewV7(), closed)

	// A failed Turn that still stands. A failure followed by another Turn, one
	// older than a day and one in a closed Session have been dealt with.
	failing := attentionSession(t, f)
	failed := attentionFailedTurn(t, f, failing, "failed", 2)
	superseded := attentionSession(t, f)
	attentionFailedTurn(t, f, superseded, "superseded", 2)
	if _, err := session.Enqueue(t.Context(), f.Pool, nil, caller, session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: superseded, IdempotencyKey: "retry", Input: json.RawMessage(`[{"type":"text","text":"again"}]`)}); err != nil {
		t.Fatal(err)
	}
	attentionFailedTurn(t, f, attentionSession(t, f), "old", 25)
	stale := attentionSession(t, f)
	attentionFailedTurn(t, f, stale, "closed", 1)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET status='cancelled' WHERE environment_id=$1 AND id=$2`, f.Environment, stale)

	// A Slack connection that lost its workspace authorization an hour ago.
	registration, installation, connection := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_app_registrations(id,org_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id) VALUES($1,$2,'app','client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$3);
 INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,workspace_name,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id,authorization_lost_at) VALUES($4,$1,$2,'app','team','bot','Acme',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['chat:write'],$3,clock_timestamp()-interval '1 hour');
 INSERT INTO slack_connections(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id) VALUES($5,$6,$7,$1,$4,$3)`, pgx.QueryExecModeSimpleProtocol, registration, org, f.User, installation, connection, f.Environment, f.Agent)

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
	path := "/api/projects/project/environments/test/attention"
	read := func(token string) api.Attention {
		t.Helper()
		w := h.request(t, http.MethodGet, path, token, "")
		if w.Code != http.StatusOK {
			t.Fatalf("attention: %d %s", w.Code, w.Body.String())
		}
		var result api.Attention
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	kinds := func(result api.Attention) []string {
		var out []string
		for _, item := range result.Items {
			out = append(out, item.Kind)
		}
		return out
	}

	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='owner' WHERE user_id=$1`, f.User)
	owner := read(h.session(t, f.User, org))
	if got := kinds(owner); len(got) != 4 || got[0] != "hold" || got[1] != "failed_turn" || got[2] != "slack_reauthorization" || got[3] != "question" {
		t.Fatalf("owner order: %v", got)
	}
	if c := owner.Counts; c.Question != 1 || c.Hold != 1 || c.FailedTurn != 1 || c.SlackReauthorization == nil || *c.SlackReauthorization != 1 {
		t.Fatalf("owner counts: %+v", c)
	}
	if item := owner.Items[0]; item.ID != hold.String() || *item.SessionID != held.String() || *item.Reason != "Approve the deploy" || item.Agent != "agent" || item.TurnID != nil {
		t.Fatalf("hold: %+v", item)
	}
	if item := owner.Items[1]; item.ID != failed.String() || *item.TurnID != failed.String() || *item.SessionID != failing.String() || *item.TurnSequence != 1 || *item.ErrorCode != "agent_error" {
		t.Fatalf("failed Turn: %+v", item)
	}
	if item := owner.Items[2]; item.ID != connection.String() || item.Agent != "agent" || item.WorkspaceName == nil || *item.WorkspaceName != "Acme" || item.SessionID != nil {
		t.Fatalf("Slack connection: %+v", item)
	}
	if item := owner.Items[3]; item.ID != question.String() || *item.TurnID != asked.TurnID.String() || *item.SessionID != f.Session.String() || *item.Prompt != "Ship the release?" {
		t.Fatalf("Question: %+v", item)
	}

	// Waiting work stays in its Environment: a second Environment of the same
	// organization lists only its own items, and the first keeps its counts.
	second, secondDeployment, secondAgent, secondSession, secondHold := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	secondRegistration, secondInstallation, secondConnection := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO environments(id,org_id,project_id,slug,name,color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at) VALUES($1,$2,$3,'second','Second','#445566',1024,32,1024,1024000,1099511627776,1099511627776,600000,1000000,1000000,1000000,clock_timestamp());
 INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$4,$5);
 INSERT INTO image_specs(environment_id,id,spec_digest,spec,seed) VALUES($1,$4,$5,'{}','{}');
 INSERT INTO templates(environment_id,id,key) VALUES($1,$4,'fixture-computer');
 INSERT INTO template_definitions(environment_id,deployment_id,template_id,image_spec_id,cpu_millis,memory_bytes,disk_bytes) VALUES($1,$4,$4,$4,1000,536870912,NULL);
 INSERT INTO agents(environment_id,id,key) VALUES($1,$6,'second-agent');
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init) VALUES($1,$6,$4,$4,false);
 INSERT INTO sessions(environment_id,id,agent_id,deployment_id,root_session_id,causal_depth) VALUES($1,$7,$6,$4,$7,0);
 INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$8,$7,'local','Second hold');
 INSERT INTO slack_app_registrations(id,org_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id) VALUES($9,$2,'second-app','second-client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$12);
 INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id,authorization_lost_at) VALUES($10,$9,$2,'second-app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['chat:write'],$12,clock_timestamp());
 INSERT INTO slack_connections(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id) VALUES($11,$1,$6,$9,$10,$12)`, pgx.QueryExecModeSimpleProtocol, second, org, project, secondDeployment, "sha256:"+strings.Repeat("2", 64), secondAgent, secondSession, secondHold, secondRegistration, secondInstallation, secondConnection, f.User)
	ownerToken := h.session(t, f.User, org)
	other := func(token, path string) api.Attention {
		t.Helper()
		w := h.request(t, http.MethodGet, path, token, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var result api.Attention
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	secondView := other(ownerToken, "/api/projects/project/environments/second/attention")
	if c := secondView.Counts; c.Question != 0 || c.Hold != 1 || c.FailedTurn != 0 || c.SlackReauthorization == nil || *c.SlackReauthorization != 1 || len(secondView.Items) != 2 ||
		secondView.Items[0].ID != secondHold.String() || secondView.Items[1].ID != secondConnection.String() {
		t.Fatalf("second Environment: %+v", secondView)
	}
	if c := read(ownerToken).Counts; c.Question != 1 || c.Hold != 1 || c.FailedTurn != 1 || *c.SlackReauthorization != 1 {
		t.Fatalf("first Environment counts changed: %+v", c)
	}
	// Another organization whose Project and Environment share the same IDs sees
	// none of it.
	otherOrg, otherOwner := h.organizationOwner(t, "elsewhere-same-ids")
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO projects(id,org_id,default_region_id,slug,name) VALUES($2,$1,'test','project','Project');
 INSERT INTO environments(id,org_id,project_id,slug,name,color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at) VALUES($3,$1,$2,'test','Test','#112233',1024,32,1024,1024000,1099511627776,1099511627776,600000,1000000,1000000,1000000,clock_timestamp())`, pgx.QueryExecModeSimpleProtocol, otherOrg, uuid.NewV7(), uuid.NewV7())
	if view := other(otherOwner, path); len(view.Items) != 0 || view.Counts.Question != 0 || view.Counts.Hold != 0 || view.Counts.FailedTurn != 0 || view.Counts.SlackReauthorization == nil || *view.Counts.SlackReauthorization != 0 {
		t.Fatalf("other organization: %+v", view)
	}

	// Members who do not manage Slack read every other kind; the Slack kind and
	// its count are absent rather than zero.
	for _, role := range []string{"developer", "viewer"} {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role=$2 WHERE user_id=$1`, f.User, role)
		w := h.request(t, http.MethodGet, path, h.session(t, f.User, org), "")
		if w.Code != http.StatusOK || bytes.Contains(w.Body.Bytes(), []byte("slack_reauthorization")) {
			t.Fatalf("%s: %d %s", role, w.Code, w.Body.String())
		}
		if got := kinds(read(h.session(t, f.User, org))); len(got) != 3 || got[0] != "hold" || got[1] != "failed_turn" || got[2] != "question" {
			t.Fatalf("%s kinds: %v", role, got)
		}
	}

	// An owner whose management authority is withdrawn after sign-in no longer
	// sees the Slack kind.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='owner' WHERE user_id=$1`, f.User)
	stillOwner := h.session(t, f.User, org)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='developer' WHERE user_id=$1`, f.User)
	if w := h.request(t, http.MethodGet, path, stillOwner, ""); w.Code != http.StatusOK || bytes.Contains(w.Body.Bytes(), []byte("slack_reauthorization")) {
		t.Fatalf("withdrawn owner: %d %s", w.Code, w.Body.String())
	}

	// Answering the Question removes it from the list.
	if _, err := session.RespondAsk(t.Context(), f.Pool, caller, session.AskAnswerRequest{EnvironmentID: f.Environment, SessionID: f.Session, TurnID: asked.TurnID, AskID: question, ResponseID: "answer", Answer: json.RawMessage(`"ship"`)}); err != nil {
		t.Fatal(err)
	}
	if after := read(h.session(t, f.User, org)); after.Counts.Question != 0 || len(after.Items) != 2 {
		t.Fatalf("answered Question still waiting: %+v", after)
	}

	// Outsiders, disabled members and API keys cannot read it.
	_, outsider := h.organizationOwner(t, "elsewhere")
	if w := h.request(t, http.MethodGet, path, outsider, ""); w.Code == http.StatusOK {
		t.Fatalf("outsider read attention: %s", w.Body.String())
	}
	member := h.session(t, f.User, org)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET disabled_at=clock_timestamp() WHERE user_id=$1`, f.User)
	if w := h.request(t, http.MethodGet, path, member, ""); w.Code == http.StatusOK {
		t.Fatalf("disabled member read attention: %s", w.Body.String())
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET disabled_at=NULL,role='owner' WHERE user_id=$1`, f.User)
	issuer := auth.Principal{OrgID: org, UserID: f.User, Kind: auth.PrincipalKindSession, SessionID: uuid.NewV7(), Role: auth.RoleOwner}
	key, err := identity.IssueAPIKey(t.Context(), f.Pool, issuer, auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: f.Environment.String()}, identity.APIKeyInput{Name: "attention read", Permissions: []auth.Permission{auth.PermissionSessionsRead}})
	if err != nil {
		t.Fatal(err)
	}
	if w := h.request(t, http.MethodGet, path, key.Raw, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("API key: %d %s", w.Code, w.Body.String())
	}
}
