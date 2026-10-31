package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

type agentDeleteFixture struct {
	h                                           httpPostgresFixture
	org, project, env, user, member, agent, dep uuid.UUID
	token, base                                 string
}

func newAgentDeleteFixture(t *testing.T) agentDeleteFixture {
	t.Helper()
	f := agentDeleteFixture{h: newHTTPPostgresFixture(t)}
	f.org, f.token = f.h.organizationOwner(t, "agent-delete")
	f.project, f.env = deploymentHTTPEnvironment(t, f.h, f.org)
	f.dep = deploymentHTTPDeployment(t, f.h, f.org, f.project, f.env)
	if err := f.h.pool.QueryRow(t.Context(), `SELECT user_id,id FROM org_members WHERE org_id=$1 AND is_owner`, f.org).Scan(&f.user, &f.member); err != nil {
		t.Fatal(err)
	}
	f.h.grant(t, f.org, f.user, f.env, "agents.delete")
	f.h.grant(t, f.org, f.user, f.env, "agents.restore")
	f.h.grant(t, f.org, f.user, f.env, "sessions.read")
	f.h.grant(t, f.org, f.user, f.env, "deployments.promote")
	if err := f.h.pool.QueryRow(t.Context(), `SELECT id FROM agents WHERE environment_id=$1`, f.env).Scan(&f.agent); err != nil {
		t.Fatal(err)
	}
	f.base = "/api/projects/deployments/environments/staging/agents/daily-report"
	return f
}
func (f agentDeleteFixture) request(t *testing.T, method, path, token, body, revision string, status int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	f.h.authenticateRequest(r, token)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if revision != "" {
		r.Header.Set("If-Match", `"`+revision+`"`)
	}
	w := httptest.NewRecorder()
	f.h.handler.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), status)
	}
	return w
}
func (f agentDeleteFixture) change(t *testing.T, restore, dry bool, revision string, status int) api.AgentChangeResponse {
	t.Helper()
	method, path, body := http.MethodDelete, f.base, ""
	if restore {
		method = http.MethodPost
		path += "/restore"
		body = `{}`
		if dry {
			body = `{"dry_run":true}`
		}
	} else if dry {
		path += "?dry_run=true"
	}
	w := f.request(t, method, path, f.token, body, revision, status)
	var result api.AgentChangeResponse
	if status == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
	return result
}
func (f agentDeleteFixture) selectedGrant(t *testing.T, permissions []string, agents ...uuid.UUID) uuid.UUID {
	t.Helper()
	role, _ := dbtest.CreateRole(t, f.h.pool, f.org, permissions...)
	grant := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.h.pool, `INSERT INTO role_grants(id,org_id,role_id,member_id,environment_id) VALUES($1,$2,$3,$4,$5)`, grant, f.org, role, f.member, f.env)
	for _, a := range agents {
		dbtest.MustExec(t, t.Context(), f.h.pool, `INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) VALUES($1,$2,$3)`, grant, f.env, a)
	}
	return grant
}
func TestAgentDeleteRestoreHTTP(t *testing.T) {
	f := newAgentDeleteFixture(t)
	sibling := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.h.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'sibling')`, f.env, sibling)
	pure := f.selectedGrant(t, []string{"sessions.create"}, f.agent)
	mixed := f.selectedGrant(t, []string{"sessions.create", "computers.execute"}, f.agent)
	multi := f.selectedGrant(t, []string{"sessions.read"}, f.agent, sibling)
	registration, connection := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.h.pool, `INSERT INTO slack_app_registrations(id,org_id,created_by_user_id,transport) VALUES($1,$2,$3,'http'); INSERT INTO slack_connections(id,environment_id,agent_id,slack_app_registration_id,created_by_user_id,org_id) VALUES($4,$5,$6,$1,$3,(SELECT org_id FROM slack_app_registrations WHERE id=$1))`, pgx.QueryExecModeSimpleProtocol, registration, f.org, f.user, connection, f.env, f.agent)
	installation := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.h.pool, `UPDATE slack_app_registrations SET client_id='test-client',app_id='test-app',credential_ciphertext=decode(repeat('11',16),'hex'),credential_nonce=decode(repeat('22',12),'hex'),credential_revision=1 WHERE id=$1; INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id) VALUES($2,$1,$3,'test-app','test-team','test-bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),'{}',$4); UPDATE slack_connections SET slack_installation_id=$2 WHERE id=$5`, pgx.QueryExecModeSimpleProtocol, registration, installation, f.org, f.user, connection)
	activation := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.h.pool, `INSERT INTO agent_triggers(environment_id,agent_id,deployment_id,key,cron,timezone,input) VALUES($1,$2,$3,'daily','* * * * *','UTC','[]'); INSERT INTO trigger_activations(environment_id,id,agent_id,deployment_id,trigger_key,active_from,active_until,next_fire_at) VALUES($1,$4,$2,$3,'daily',clock_timestamp()-interval '2 minutes',clock_timestamp()-interval '1 second',clock_timestamp()-interval '1 minute')`, pgx.QueryExecModeSimpleProtocol, f.env, f.agent, f.dep, activation)
	preview := f.change(t, false, true, "", 200)
	if !preview.CanApply || preview.Changed || preview.Effects.SelectedAgentEdgesRemoved != 3 || preview.Effects.IneffectiveAssignmentsRemoved != 1 || preview.Effects.MixedAssignmentsPreserved != 1 || preview.Effects.SlackConnectionDisconnected == nil {
		t.Fatalf("preview %+v", preview)
	}
	f.change(t, false, false, "", 428)
	result := f.change(t, false, false, "1", 200)
	if !result.Changed || result.DeletedAt == nil || result.Revision != 2 || result.Effects.TriggerActivationsSealed != 1 {
		t.Fatalf("delete %+v", result)
	}
	var good bool
	if err := f.h.pool.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM role_grants WHERE id=$1) AND (SELECT revision=2 AND NOT all_agents FROM role_grants WHERE id=$2) AND NOT EXISTS(SELECT 1 FROM role_grant_agents WHERE grant_id=$2) AND (SELECT revision=2 FROM role_grants WHERE id=$3) AND (SELECT count(*)=1 FROM role_grant_agents WHERE grant_id=$3) AND (SELECT revoked_at IS NOT NULL FROM slack_connections WHERE id=$4) AND (SELECT retired_at IS NOT NULL AND credential_ciphertext IS NULL FROM slack_app_registrations WHERE id=$5) AND (SELECT next_fire_at=active_until FROM trigger_activations WHERE id=$6)`, pure, mixed, multi, connection, registration, activation).Scan(&good); err != nil || !good {
		t.Fatalf("cleanup=%v %v", good, err)
	}
	if err := f.h.pool.QueryRow(t.Context(), `SELECT count(*)=1 FROM audit_events WHERE action='slack_connection.disconnected' AND target_id=$1 AND details->>'cause_agent_id'=$2 AND details->>'reason'='agent_deleted'`, connection, f.agent.String()).Scan(&good); err != nil || !good {
		t.Fatalf("missing disconnection cause: %v %v", good, err)
	}
	f.request(t, http.MethodGet, f.base+"/status", f.token, "", "", 200)
	f.request(t, http.MethodGet, strings.TrimSuffix(f.base, "/daily-report")+"?view=deleted", f.token, "", "", 200)
	f.request(t, http.MethodPost, "/api/projects/deployments/environments/staging/deployments/"+f.dep.String()+"/promote", f.token, `{}`, "", 409)
	if r := f.change(t, false, false, "2", 200); r.Changed || r.Revision != 2 {
		t.Fatalf("noop %+v", r)
	}
	f.change(t, true, true, "", 200)
	if r := f.change(t, true, false, "2", 200); !r.Changed || r.DeletedAt != nil || r.Revision != 3 {
		t.Fatalf("restore %+v", r)
	}
	f.change(t, false, false, "1", 412)
	f.request(t, http.MethodPost, f.base+"/restore", f.token, "", "3", 200)
	if err := f.h.pool.QueryRow(t.Context(), `SELECT disconnected_at IS NOT NULL AND credential_ciphertext IS NULL AND credential_nonce IS NULL FROM slack_installations WHERE id=$1`, installation).Scan(&good); err != nil || !good {
		t.Fatalf("installation credential resurrection %v %v", good, err)
	}

	if err := f.h.pool.QueryRow(t.Context(), `SELECT (SELECT revoked_at IS NOT NULL FROM slack_connections WHERE id=$1) AND NOT EXISTS(SELECT 1 FROM role_grant_agents WHERE agent_id=$2) AND (SELECT next_fire_at=active_until FROM trigger_activations WHERE id=$3)`, connection, f.agent, activation).Scan(&good); err != nil || !good {
		t.Fatalf("restore resurrected effects: %v %v", good, err)
	}
}

func TestAgentDeletePrerequisitesAndRollback(t *testing.T) {
	f := newAgentDeleteFixture(t)
	dbtest.MustExec(t, t.Context(), f.h.pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.env, f.dep)
	if r := f.change(t, false, true, "", 200); r.CanApply || len(r.Blockers) != 1 || r.Blockers[0].Code != "defined_in_current" {
		t.Fatalf("defined %+v", r)
	}
	f.change(t, false, false, "1", 409)
	dbtest.MustExec(t, t.Context(), f.h.pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.env)
	sess := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.h.pool, `INSERT INTO sessions(environment_id,id,agent_id,deployment_id,root_session_id,causal_depth,origin_user_id) VALUES($1,$2,$3,$4,$2,0,$5)`, f.env, sess, f.agent, f.dep, f.user)
	// An open Session does not block deletion; the preview counts it.
	if r := f.change(t, false, true, "", 200); !r.CanApply || len(r.Blockers) != 0 || r.Effects.SessionsCancelled != 1 {
		t.Fatalf("live %+v", r)
	}
	grant := f.selectedGrant(t, []string{"sessions.create"}, f.agent)
	dbtest.MustExec(t, t.Context(), f.h.pool, `CREATE FUNCTION reject_agent_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='agent.deleted' THEN RAISE EXCEPTION 'injected audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_agent_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_agent_audit()`)
	f.change(t, false, false, "1", 500)
	var intact bool
	if err := f.h.pool.QueryRow(t.Context(), `SELECT (SELECT deleted_at IS NULL AND revision=1 FROM agents WHERE id=$1) AND EXISTS(SELECT 1 FROM role_grant_agents WHERE grant_id=$2 AND agent_id=$1) AND (SELECT status='open' FROM sessions WHERE id=$3)`, f.agent, grant, sess).Scan(&intact); err != nil || !intact {
		t.Fatalf("partial cleanup %v %v", intact, err)
	}
	dbtest.MustExec(t, t.Context(), f.h.pool, `DROP TRIGGER reject_agent_audit ON audit_events`)
	if r := f.change(t, false, false, "1", 200); !r.Changed || r.Effects.SessionsCancelled != 1 {
		t.Fatalf("delete %+v", r)
	}
	var cancelled bool
	if err := f.h.pool.QueryRow(t.Context(), `SELECT status='closed' AND close_how='cancel' FROM sessions WHERE id=$1`, sess).Scan(&cancelled); err != nil || !cancelled {
		t.Fatalf("deletion did not cancel the Session: %v %v", cancelled, err)
	}
}

func TestAgentDeleteScopedManagerVisibilityAndSelfLockout(t *testing.T) {
	f := newAgentDeleteFixture(t)
	user := f.h.user(t, "Scoped manager")
	member := f.h.member(t, f.org, user, "member")
	token := f.h.session(t, user, f.org)
	scoped := f
	scoped.member = member
	scoped.token = token
	scoped.selectedGrant(t, []string{"agents.delete"}, f.agent)
	sess := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.h.pool, `INSERT INTO sessions(environment_id,id,agent_id,deployment_id,root_session_id,causal_depth,origin_user_id) VALUES($1,$2,$3,$4,$2,0,$5)`, f.env, sess, f.agent, f.dep, f.user)
	preview := scoped.change(t, false, true, "", 200)
	if !preview.CanApply || len(preview.Blockers) != 0 || preview.Effects.SessionsCancelled != 1 || len(preview.Warnings) != 1 {
		t.Fatalf("scoped preview %+v", preview)
	}
	scoped.change(t, false, false, "1", 200)
	scoped.change(t, true, true, "", 404)
	scoped.request(t, http.MethodGet, f.base+"/status", token, "", "", 404)
	f.change(t, true, false, "2", 200)
	scoped.change(t, false, true, "", 404)
}

func TestAgentLifecycleRequiresOnlyItsDedicatedPermission(t *testing.T) {
	f := newAgentDeleteFixture(t)
	user := f.h.user(t, "Agent manager")
	f.h.member(t, f.org, user, "member")
	f.h.grant(t, f.org, user, f.env, "agents.delete")
	token := f.h.session(t, user, f.org)
	f.request(t, http.MethodDelete, f.base+"?dry_run=true", token, "", "", 200)
	f.request(t, http.MethodPost, f.base+"/restore", token, `{"dry_run":true}`, "", 403)
	restorer := f.h.user(t, "Agent restorer")
	f.h.member(t, f.org, restorer, "member")
	f.h.grant(t, f.org, restorer, f.env, "agents.restore")
	restoreToken := f.h.session(t, restorer, f.org)
	f.request(t, http.MethodDelete, f.base+"?dry_run=true", restoreToken, "", "", 403)
	f.request(t, http.MethodDelete, f.base, token, "", "1", 200)
	f.request(t, http.MethodPost, f.base+"/restore", restoreToken, `{"dry_run":true}`, "", 200)
	f.request(t, http.MethodPost, f.base+"/restore", restoreToken, `{}`, "2", 200)
	admin := f.h.user(t, "Access manager")
	f.h.member(t, f.org, admin, "admin")
	f.request(t, http.MethodDelete, f.base+"?dry_run=true", f.h.session(t, admin, f.org), "", "", 404)
	for _, query := range []string{"?dry_run=yes", "?dry_run=true&dry_run=false", "?force=true"} {
		f.request(t, http.MethodDelete, f.base+query, f.token, "", "1", 400)
	}
	f.request(t, http.MethodDelete, f.base, f.token, `{}`, "1", 400)
	f.request(t, http.MethodPost, f.base+"/restore", f.token, `{"unexpected":true}`, "1", 400)
	f.request(t, http.MethodPost, f.base+"/restore?dry_run=true", f.token, `{}`, "1", 400)
	_, foreign := f.h.organizationOwner(t, "foreign-delete")
	f.request(t, http.MethodDelete, f.base+"?dry_run=true", foreign, "", "", 400)
}

func TestAgentDeleteSerializesPromotion(t *testing.T) {
	f := newAgentDeleteFixture(t)
	start := make(chan struct{})
	results := make(chan int, 2)
	for _, operation := range []string{"delete", "promote"} {
		go func() {
			<-start
			method, path, body := http.MethodDelete, f.base, ""
			if operation == "promote" {
				method = http.MethodPost
				path = "/api/projects/deployments/environments/staging/deployments/" + f.dep.String() + "/promote"
				body = `{}`
			}
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			f.h.authenticateRequest(r, f.token)
			r.Header.Set("If-Match", `"1"`)
			w := httptest.NewRecorder()
			f.h.handler.ServeHTTP(w, r)
			results <- w.Code
		}()
	}
	close(start)
	a, b := <-results, <-results
	if !((a == 200 && b == 409) || (a == 409 && b == 200)) {
		t.Fatalf("race outcomes %d %d", a, b)
	}
	var consistent bool
	if err := f.h.pool.QueryRow(t.Context(), `SELECT NOT (a.deleted_at IS NOT NULL AND e.current_deployment_id IS NOT DISTINCT FROM $3) FROM agents a JOIN environments e ON e.id=a.environment_id WHERE a.environment_id=$1 AND a.id=$2`, f.env, f.agent, f.dep).Scan(&consistent); err != nil || !consistent {
		t.Fatalf("deleted current Agent: %v %v", consistent, err)
	}
}

func TestDeletedAgentPromotionReportsKeysBeforeSlackPreflight(t *testing.T) {
	f := newAgentDeleteFixture(t)
	f.change(t, false, false, "1", 200)
	dbtest.MustExec(t, t.Context(), f.h.pool, `INSERT INTO agent_triggers(environment_id,agent_id,deployment_id,key,cron,timezone,input,slack_channel_id) VALUES($1,$2,$3,'daily','* * * * *','UTC','[]','CEXAMPLE')`, f.env, f.agent, f.dep)
	w := f.request(t, http.MethodPost, "/api/projects/deployments/environments/staging/deployments/"+f.dep.String()+"/promote", f.token, `{}`, "", 409)
	if !strings.Contains(w.Body.String(), `"agent_deleted"`) || !strings.Contains(w.Body.String(), `"agents":["daily-report"]`) || !strings.Contains(w.Body.String(), `"count":1`) {
		t.Fatalf("missing blockers: %s", w.Body)
	}
}

func TestAgentDeleteRechecksNewDependencies(t *testing.T) {
	for _, dependency := range []string{"grant", "slack"} {
		t.Run(dependency, func(t *testing.T) {
			f := newAgentDeleteFixture(t)
			tx, err := f.h.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			var locked uuid.UUID
			if err = tx.QueryRow(t.Context(), `SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, f.env).Scan(&locked); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			go func() {
				r := httptest.NewRequest(http.MethodDelete, f.base, nil).WithContext(ctx)
				f.h.authenticateRequest(r, f.token)
				r.Header.Set("If-Match", `"1"`)
				w := httptest.NewRecorder()
				f.h.handler.ServeHTTP(w, r)
				done <- w
			}()
			for {
				var waiting bool
				if err = f.h.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT current_deployment_id,max_outstanding_admissions%')`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case w := <-done:
					t.Fatalf("delete did not wait: %d %s", w.Code, w.Body)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(5 * time.Millisecond):
				}
			}
			if dependency == "grant" {
				user := f.h.user(t, "Other access manager")
				member := f.h.member(t, f.org, user, "admin")
				role, _ := dbtest.CreateRole(t, f.h.pool, f.org, "sessions.create")
				input := map[string]any{"role_id": role, "expected_role_revision": 1, "subject": map[string]any{"type": "member", "member_id": member}, "project_id": f.project, "environment_id": f.env, "agents_mode": "selected", "agent_ids": []uuid.UUID{f.agent}}
				body, _ := json.Marshal(input)
				f.request(t, http.MethodPost, "/api/role-grants", f.h.session(t, user, f.org), string(body), "", 201)
			} else {
				// A connection owner that already holds the admission fence can commit a
				// newly discovered connection before deletion acquires that fence.
				registration, connection := uuid.NewV7(), uuid.NewV7()
				if _, err = tx.Exec(ctx, `INSERT INTO slack_app_registrations(id,org_id,created_by_user_id,transport) VALUES($1,$2,$3,'http'); INSERT INTO slack_connections(id,environment_id,agent_id,slack_app_registration_id,created_by_user_id,org_id) VALUES($4,$5,$6,$1,$3,(SELECT org_id FROM slack_app_registrations WHERE id=$1))`, pgx.QueryExecModeSimpleProtocol, registration, f.org, f.user, connection, f.env, f.agent); err != nil {
					t.Fatal(err)
				}
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			w := <-done
			if w.Code != 409 || !strings.Contains(w.Body.String(), "agent_dependencies_changed") {
				t.Fatalf("new dependency not rejected: %d %s", w.Code, w.Body)
			}
			f.change(t, false, false, "1", 200)
		})
	}
}

func TestAgentDeleteInvalidatesGrantEditorsAndPreservesEmptyMixedGrant(t *testing.T) {
	f := newAgentDeleteFixture(t)
	grant := f.selectedGrant(t, []string{"sessions.create", "computers.execute"}, f.agent)
	var role uuid.UUID
	if err := f.h.pool.QueryRow(t.Context(), `SELECT role_id FROM role_grants WHERE id=$1`, grant).Scan(&role); err != nil {
		t.Fatal(err)
	}
	f.change(t, false, false, "1", 200)
	f.change(t, true, false, "2", 200)
	input := map[string]any{"role_id": role, "expected_role_revision": 1, "subject": map[string]any{"type": "member", "member_id": f.member}, "project_id": f.project, "environment_id": f.env, "agents_mode": "selected", "agent_ids": []uuid.UUID{f.agent}}
	body, _ := json.Marshal(input)
	f.request(t, http.MethodPatch, "/api/role-grants/"+grant.String(), f.token, string(body), "1", 412)
	input["agent_ids"] = []uuid.UUID{}
	body, _ = json.Marshal(input)
	f.request(t, http.MethodPatch, "/api/role-grants/"+grant.String(), f.token, string(body), "2", 200)
}

func TestAgentDeleteReadsRoleAfterConcurrentEdit(t *testing.T) {
	f := newAgentDeleteFixture(t)
	grant := f.selectedGrant(t, []string{"sessions.create"}, f.agent)
	tx, err := f.h.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var role uuid.UUID
	if err = tx.QueryRow(t.Context(), `SELECT r.id FROM roles r JOIN role_grants g ON g.role_id=r.id WHERE g.id=$1 FOR UPDATE OF r`, grant).Scan(&role); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodDelete, f.base, nil).WithContext(ctx)
		f.h.authenticateRequest(r, f.token)
		r.Header.Set("If-Match", `"1"`)
		w := httptest.NewRecorder()
		f.h.handler.ServeHTTP(w, r)
		done <- w
	}()
	for {
		var waiting bool
		if err = f.h.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT r.id FROM roles r WHERE%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case w := <-done:
			t.Fatalf("delete did not wait: %d %s", w.Code, w.Body)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE roles SET permissions=ARRAY['sessions.create','computers.execute'],revision=revision+1 WHERE id=$1`, role); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	w := <-done
	if w.Code != 200 {
		t.Fatalf("delete %d %s", w.Code, w.Body)
	}
	var preserved bool
	if err = f.h.pool.QueryRow(ctx, `SELECT revision=2 AND NOT EXISTS(SELECT 1 FROM role_grant_agents WHERE grant_id=$1) FROM role_grants WHERE id=$1`, grant).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("edited mixed grant lost: %v %v", preserved, err)
	}
}

func TestAgentDeleteRevocationWhileWaitingReturnsPermissionRequired(t *testing.T) {
	f := newAgentDeleteFixture(t)
	tx, err := f.h.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SELECT id FROM org_members WHERE id=$1 FOR UPDATE`, f.member); err != nil {
		t.Fatal(err)
	}
	var blocker int
	if err := tx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodDelete, f.base, nil).WithContext(ctx)
		f.h.authenticateRequest(r, f.token)
		r.Header.Set("If-Match", `"1"`)
		w := httptest.NewRecorder()
		f.h.handler.ServeHTTP(w, r)
		done <- w
	}()
	for {
		var waiting bool
		if err := f.h.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case w := <-done:
			t.Fatalf("delete did not wait: %d %s", w.Code, w.Body)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM role_grants g USING roles r WHERE r.id=g.role_id AND g.member_id=$1 AND 'agents.delete'=ANY(r.permissions)`, f.member); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	w := <-done
	if w.Code != http.StatusForbidden || decodeHTTPError(t, w.Body.Bytes()).Code != "permission_required" {
		t.Fatalf("revoked deletion: %d %s", w.Code, w.Body)
	}
	var unchanged bool
	if err := f.h.pool.QueryRow(t.Context(), `SELECT deleted_at IS NULL AND revision=1 FROM agents WHERE id=$1`, f.agent).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("revoked delete persisted: %v %v", unchanged, err)
	}
}
