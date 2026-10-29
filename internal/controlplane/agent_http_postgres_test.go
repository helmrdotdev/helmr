package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"
)

func TestAgentHTTPAdmission(t *testing.T) {
	f := sessiontest.New(t)
	sessiontest.ObserveProvisioning(t, f)
	dbtest.MustExec(t, t.Context(), f.Pool, `
 UPDATE image_specs SET seed=jsonb_build_object('profile',$2::text) WHERE environment_id=$1;
 UPDATE template_definitions SET cpu_millis=1000,memory_bytes=536870912 WHERE environment_id=$1;
 UPDATE org_members SET is_owner=true WHERE user_id=$3;
 `, pgx.QueryExecModeSimpleProtocol, f.Environment, definition.ComputerSeedProfile, f.User)
	var orgID, projectID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&orgID, &projectID); err != nil {
		t.Fatal(err)
	}
	queries := db.New(f.Pool)
	cfg := completeServerConfig(t)
	cfg.AppOrigin = &url.URL{Scheme: "https", Host: "console.example.test"}
	cfg.DB, cfg.TX, cfg.Auth = queries, f.Pool, identity.NewAPIKeyAuthenticator(f.Pool)
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewKeys(cfg.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	httpFixture := httpPostgresFixture{pool: f.Pool, queries: queries, handler: handler, keys: keys, browserOrganizations: map[string]uuid.UUID{}, operatorTokens: map[string]bool{}, origin: cfg.AppOrigin.String()}
	owner := httpFixture.session(t, f.User, orgID)
	base := "/api/projects/project/environments/test"
	start := base + "/sessions"
	input := `{"input":[{"type":"text","text":"{\"work\":1}"}],"key":"conversation","idempotency_key":"first"}`
	call := func(path, token, body string, status int) []byte {
		t.Helper()
		if path == start || path == "/v1/sessions" {
			var value map[string]json.RawMessage
			if json.Unmarshal([]byte(body), &value) == nil && value != nil && len(value) > 0 {
				value["agent"] = json.RawMessage(`"agent"`)
				raw, _ := json.Marshal(value)
				body = string(raw)
			}
		}
		response := httpFixture.request(t, http.MethodPost, path, token, body)
		if response.Code != status {
			t.Fatalf("%s = %d %s, want %d", path, response.Code, response.Body.String(), status)
		}
		return response.Body.Bytes()
	}
	expectCode := func(raw []byte, code string) {
		t.Helper()
		var response api.HTTPErrorResponse
		if err := json.Unmarshal(raw, &response); err != nil || response.Error.Code != code {
			t.Fatalf("error code: %s, want %s (%v)", raw, code, err)
		}
	}
	decode := func(raw []byte) api.CreateSessionResponse {
		t.Helper()
		var receipt api.CreateSessionResponse
		if err := json.Unmarshal(raw, &receipt); err != nil {
			t.Fatal(err)
		}
		return receipt
	}
	call(start, "", input, http.StatusUnauthorized)
	viewerID := httpFixture.user(t, "Viewer")
	httpFixture.member(t, orgID, viewerID, "member")
	viewer := httpFixture.session(t, viewerID, orgID)
	httpFixture.grant(t, orgID, viewerID, f.Environment, "sessions.read")
	call(start, viewer, input, http.StatusForbidden)
	for _, body := range []string{`{"input":[{"type":"text","text":"{}"}],"caller_id":"fake"}`, `{"input":[{"type":"text","text":"{}"}],"environment_id":"fake"}`, `{"input":[{"type":"text","text":"{}"}],"computer_id":"bad"}`, `{"input":{}} {}`, `{}`, `null`} {
		call(start, owner, body, http.StatusBadRequest)
	}
	first := decode(call(start, owner, input, http.StatusOK))
	if first.Turn == nil || first.Turn.Sequence != 1 || first.SessionID == "" || first.Turn.TurnID == "" {
		t.Fatalf("incomplete receipt: %+v", first)
	}
	var pendingPreparation bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT c.image_build_id IS NOT NULL AND c.id<>$2 AND s.deployment_id=$3 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=s.environment_id AND l.computer_id=s.computer_id) FROM sessions s JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id) WHERE s.id=$1`, first.SessionID, f.Computer, f.Deployment).Scan(&pendingPreparation); err != nil || !pendingPreparation {
		t.Fatalf("fresh admission did not queue preparation: %v %v", pendingPreparation, err)
	}
	if retry := decode(call(start, owner, input, http.StatusOK)); !reflect.DeepEqual(retry, first) {
		t.Fatalf("retry changed: %+v / %+v", first, retry)
	}
	expectCode(call(start, owner, `{"input":[{"type":"text","text":"2"}],"key":"conversation","idempotency_key":"first"}`, http.StatusConflict), "idempotency_conflict")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET admission_tokens=0,admission_refilled_at=clock_timestamp()+interval '1 day' WHERE id=$1`, f.Environment)
	expectCode(call(start, owner, `{"input":[{"type":"text","text":"2"}],"idempotency_key":"backpressure"}`, http.StatusConflict), "admission_unavailable")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET admission_tokens=1000000,admission_refilled_at=clock_timestamp() WHERE id=$1`, f.Environment)
	call(start, owner, `{"input":[{"type":"text","text":"2"}],"idempotency_key":"backpressure"}`, http.StatusOK)
	// An occupied key conflicts; enqueue preserves the original pin.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.Environment)
	expectCode(call(start, owner, `{"input":[{"type":"text","text":"2"}],"key":"conversation","idempotency_key":"second"}`, http.StatusConflict), "idempotency_conflict")
	enqueue := base + "/sessions/" + first.SessionID + "/enqueue"
	var third, replayEnqueue api.TurnAdmission
	if err := json.Unmarshal(call(enqueue, owner, `{"input":[{"type":"text","text":"3"}],"idempotency_key":"third"}`, http.StatusOK), &third); err != nil || third.SessionID != first.SessionID || third.Sequence != 2 {
		t.Fatalf("enqueue: %+v %v", third, err)
	}
	if err := json.Unmarshal(call(enqueue, owner, `{"input":[{"type":"text","text":"3"}],"idempotency_key":"third"}`, http.StatusOK), &replayEnqueue); err != nil || replayEnqueue != third {
		t.Fatalf("enqueue retry: %+v %v", replayEnqueue, err)
	}
	call(enqueue, viewer, `{"input":[{"type":"text","text":"4"}]}`, http.StatusForbidden)
	call(base+"/sessions/"+uuid.NewV7().String()+"/enqueue", owner, `{"input":[{"type":"text","text":"4"}]}`, http.StatusForbidden)
	_, otherOwner := httpFixture.organizationOwner(t, "other-org")
	call(start, otherOwner, input, http.StatusBadRequest)
	var actualCaller string
	if err := f.Pool.QueryRow(t.Context(), `SELECT caller_id::text FROM turns WHERE id=$1`, first.Turn.TurnID).Scan(&actualCaller); err != nil || actualCaller != f.User.String() {
		t.Fatalf("wrong caller: %s %v", actualCaller, err)
	}

	scope := auth.Scope{OrgID: orgID, ProjectID: projectID.String(), EnvironmentID: f.Environment.String()}
	issuer := f.Principal(t)
	issue := func(permissions ...auth.Permission) string {
		t.Helper()
		key, err := issueAPIKeyWithRole(t, t.Context(), f.Pool, issuer, scope, testAPIKeyInput{Name: uuid.NewV7().String(), RolePermissions: permissions})
		if err != nil {
			t.Fatal(err)
		}
		return key.Raw
	}
	otherEnvironment := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO environments(id,org_id,project_id,slug,name,color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at) VALUES($1,$2,$3,'other','Other','#112233',1024,32,1024,1024000,1099511627776,1099511627776,600000,1000000,1000000,1000000,clock_timestamp())`, otherEnvironment, orgID, projectID)
	otherScope := scope
	otherScope.EnvironmentID = otherEnvironment.String()
	dbtest.GrantMember(t, f.Pool, orgID, f.User, &otherEnvironment, "api_keys.create")
	issuer = f.Principal(t)
	otherKey, err := issueAPIKeyWithRole(t, t.Context(), f.Pool, issuer, otherScope, testAPIKeyInput{Name: "other", RolePermissions: []auth.Permission{auth.PermissionSessionsSend}})
	if err != nil {
		t.Fatal(err)
	}
	call("/v1/sessions/"+first.SessionID+"/enqueue", otherKey.Raw, `{"input":[{"type":"text","text":"4"}]}`, http.StatusForbidden)
	call(fmt.Sprintf("/api/projects/project/environments/other/sessions/%s/enqueue", first.SessionID), owner, `{"input":[{"type":"text","text":"4"}]}`, http.StatusForbidden)
	starter, sender := issue(auth.PermissionAgentsUse), issue(auth.PermissionAgentsUse, auth.PermissionSessionsSend)
	call("/v1/sessions", issue(auth.PermissionSessionsRead), input, http.StatusForbidden)
	call("/v1/sessions/"+first.SessionID+"/enqueue", starter, `{"input":[{"type":"text","text":"4"}]}`, http.StatusForbidden)
	call("/v1/sessions/"+first.SessionID+"/enqueue", sender, `{"input":[{"type":"text","text":"4"}]}`, http.StatusOK)
	call("/v1/sessions", starter, `{"input":[{"type":"text","text":"{}"}],"environment_id":"override"}`, http.StatusBadRequest)
	call(start, starter, input, http.StatusUnauthorized)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.Environment, f.Deployment)
	keyAdmission := decode(call("/v1/sessions", starter, `{"input":[{"type":"text","text":"5"}],"idempotency_key":"key-start"}`, http.StatusOK))
	var valid bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT t.caller_kind='api_key' AND t.caller_id=k.id AND t.environment_id=k.environment_id FROM turns t JOIN api_keys k ON k.token_hash=$2 WHERE t.id=$1`, keyAdmission.Turn.TurnID, auth.HashAPIKey(starter)).Scan(&valid); err != nil || !valid {
		t.Fatalf("API key identity: %v %v", valid, err)
	}

	// Lifetime policy survives both HTTP transports; preservation is separately scoped.
	never := decode(call("/v1/sessions", starter, `{"agent":"agent","close_after_idle_ms":null,"idempotency_key":"never"}`, http.StatusOK))
	if never.CloseAfterIdleMs != nil {
		t.Fatalf("never policy: %+v", never)
	}
	finite := decode(call(start, owner, `{"agent":"agent","close_after_idle_ms":12000}`, http.StatusOK))
	if finite.CloseAfterIdleMs == nil || *finite.CloseAfterIdleMs != 12000 {
		t.Fatalf("finite policy: %+v", finite)
	}
	for _, body := range []string{`{"input":[],"close_after_idle_ms":0}`, `{"input":[],"close_after_idle_ms":1.5}`} {
		call(start, owner, body, http.StatusBadRequest)
	}
	preserver := issue(auth.PermissionComputersUpdateRetention)
	computerPath := "/v1/computers/" + f.Computer.String()
	for _, tc := range []struct {
		token, body string
		status      int
	}{
		{sender, `{"delete_after_unused_ms":null}`, http.StatusForbidden},
		{preserver, `{}`, http.StatusBadRequest},
		{preserver, `{"delete_after_unused_ms":0}`, http.StatusBadRequest},
		{preserver, `{"delete_after_unused_ms":null}`, http.StatusOK},
	} {
		response := httpFixture.request(t, http.MethodPatch, computerPath, tc.token, tc.body)
		if response.Code != tc.status {
			t.Fatalf("preservation: %d %s, want %d", response.Code, response.Body.String(), tc.status)
		}
	}
	var stored *int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT delete_after_unused_ms FROM computers WHERE id=$1`, f.Computer).Scan(&stored); err != nil || stored != nil {
		t.Fatalf("preservation persisted: %v %v", stored, err)
	}
	response := httpFixture.request(t, http.MethodPatch, base+"/computers/"+f.Computer.String(), owner, `{"delete_after_unused_ms":5000}`)
	if response.Code != http.StatusOK {
		t.Fatalf("session preservation: %d %s", response.Code, response.Body.String())
	}

	// Explicit recovery releases only its named hold through the same owner on
	// both transports. Acceptance must not claim that execution has resumed.
	systemHold, userHold, laterHold := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `
 INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','session_continuation_lost');
 INSERT INTO session_holds(environment_id,id,session_id,scope,reason,issuer_kind,issuer_id) VALUES($1,$4,$3,'subtree','operator hold','user',$5);
 `, pgx.QueryExecModeSimpleProtocol, f.Environment, systemHold, first.SessionID, userHold, f.User)
	resumePath := base + "/sessions/" + first.SessionID + "/resume"
	keyResumePath := "/v1/sessions/" + first.SessionID + "/resume"
	resumeBody := fmt.Sprintf(`{"hold_id":%q,"idempotency_key":"explicit-recovery"}`, systemHold.String())
	userResumeBody := fmt.Sprintf(`{"hold_id":%q,"idempotency_key":"explicit-recovery"}`, userHold.String())
	resumer := issue(auth.PermissionAgentsUse, auth.PermissionSessionsResume)
	call(resumePath, "", resumeBody, http.StatusUnauthorized)
	call(resumePath, viewer, resumeBody, http.StatusForbidden)
	call(keyResumePath, starter, resumeBody, http.StatusForbidden)
	call(keyResumePath, resumer, userResumeBody, http.StatusForbidden)
	call(fmt.Sprintf("/api/projects/project/environments/other/sessions/%s/resume", first.SessionID), owner, resumeBody, http.StatusForbidden)
	call("/v1/sessions/"+f.Session.String()+"/resume", resumer, resumeBody, http.StatusForbidden)
	for _, body := range []string{`{}`, `{"hold_id":"bad","idempotency_key":"r"}`, `{"hold_id":"00000000-0000-0000-0000-000000000000","idempotency_key":"r"}`, fmt.Sprintf(`{"hold_id":%q}`, systemHold.String()), fmt.Sprintf(`{"hold_id":%q,"idempotency_key":"r","caller_id":"fake"}`, systemHold.String())} {
		call(resumePath, owner, body, http.StatusBadRequest)
	}
	call(keyResumePath, resumer, fmt.Sprintf(`{"hold_id":%q,"idempotency_key":"absent"}`, uuid.NewV7().String()), http.StatusForbidden)
	for _, transport := range []struct{ path, token, body, hold string }{
		{keyResumePath, resumer, resumeBody, systemHold.String()},
		{resumePath, owner, userResumeBody, userHold.String()},
	} {
		var receipt, retried api.SessionResumeReceipt
		if err := json.Unmarshal(call(transport.path, transport.token, transport.body, http.StatusOK), &receipt); err != nil || receipt.ID == "" || receipt.SessionID != first.SessionID || receipt.HoldID != transport.hold || receipt.Status != "accepted" {
			t.Fatalf("resume receipt: %+v %v", receipt, err)
		}
		var generation, afterRetry int64
		if err := f.Pool.QueryRow(t.Context(), `SELECT authority_generation FROM sessions WHERE id=$1`, first.SessionID).Scan(&generation); err != nil {
			t.Fatal(err)
		}
		if transport.hold == systemHold.String() {
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','later recovery hold')`, f.Environment, laterHold, first.SessionID)
		}
		if err := json.Unmarshal(call(transport.path, transport.token, transport.body, http.StatusOK), &retried); err != nil || retried != receipt {
			t.Fatalf("resume retry: %+v %v", retried, err)
		}
		if err := f.Pool.QueryRow(t.Context(), `SELECT authority_generation FROM sessions WHERE id=$1`, first.SessionID).Scan(&afterRetry); err != nil || afterRetry != generation {
			t.Fatalf("resume retry advanced authority: %d -> %d (%v)", generation, afterRetry, err)
		}
		if transport.hold == systemHold.String() {
			var exact bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT released_at IS NOT NULL FROM session_holds WHERE id=$1) AND (SELECT released_at IS NULL FROM session_holds WHERE id=$2) AND (SELECT released_at IS NULL FROM session_holds WHERE id=$4) AND NOT EXISTS(SELECT 1 FROM turns WHERE session_id=$3 AND status<>'queued')`, systemHold, userHold, first.SessionID, laterHold).Scan(&exact); err != nil || !exact {
				t.Fatalf("resume affected another hold or queued work: %v %v", exact, err)
			}
			expectCode(call(keyResumePath, resumer, userResumeBody, http.StatusConflict), "idempotency_conflict")
		}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET revoked_at=clock_timestamp() WHERE token_hash=$1`, auth.HashAPIKey(resumer))
	call(keyResumePath, resumer, resumeBody, http.StatusUnauthorized)

	// An idle Session can be interrupted without choosing a Turn. Transport
	// retries retain the original hold even after an exact resume releases it.
	toInterrupt := decode(call(start, owner, `{"input":[{"type":"text","text":"1"}],"idempotency_key":"interrupt-target"}`, http.StatusOK))
	interruptPath := base + "/sessions/" + toInterrupt.SessionID + "/interrupt"
	keyInterruptPath := "/v1/sessions/" + toInterrupt.SessionID + "/interrupt"
	interruptBody := `{"idempotency_key":"interrupt-retained"}`
	interrupter := issue(auth.PermissionSessionsInterrupt)
	otherInterrupter, err := issueAPIKeyWithRole(t, t.Context(), f.Pool, issuer, otherScope, testAPIKeyInput{Name: "other interrupt", RolePermissions: []auth.Permission{auth.PermissionSessionsInterrupt}})
	if err != nil {
		t.Fatal(err)
	}
	call(interruptPath, "", interruptBody, http.StatusUnauthorized)
	call(interruptPath, viewer, interruptBody, http.StatusForbidden)
	call(keyInterruptPath, starter, interruptBody, http.StatusForbidden)
	call(keyInterruptPath, otherInterrupter.Raw, interruptBody, http.StatusForbidden)
	call(fmt.Sprintf("/api/projects/project/environments/other/sessions/%s/interrupt", toInterrupt.SessionID), owner, interruptBody, http.StatusForbidden)
	for _, body := range []string{`{}`, `{"idempotency_key":"interrupt","caller_id":"fake"}`, `{"idempotency_key":"interrupt","turn_id":"fake"}`, `{"idempotency_key":"interrupt","scope":"self"}`} {
		call(interruptPath, owner, body, http.StatusBadRequest)
	}
	for _, transport := range []struct{ path, token string }{{interruptPath, owner}, {keyInterruptPath, interrupter}} {
		var receipt, replay api.SessionInterruptReceipt
		if err := json.Unmarshal(call(transport.path, transport.token, interruptBody, http.StatusOK), &receipt); err != nil || receipt.ID == "" || receipt.SessionID != toInterrupt.SessionID || receipt.HoldID == "" || receipt.Status != "accepted" {
			t.Fatalf("interrupt receipt: %+v %v", receipt, err)
		}
		if err := json.Unmarshal(call(transport.path, transport.token, interruptBody, http.StatusOK), &replay); err != nil || replay != receipt {
			t.Fatalf("interrupt retry: %+v %v", replay, err)
		}
		if transport.token == owner {
			call(base+"/sessions/"+toInterrupt.SessionID+"/resume", owner, fmt.Sprintf(`{"idempotency_key":"resume-http-interrupt","hold_id":%q}`, receipt.HoldID), http.StatusOK)
			if err := json.Unmarshal(call(transport.path, transport.token, interruptBody, http.StatusOK), &replay); err != nil || replay != receipt {
				t.Fatalf("released interrupt retry: %+v %v", replay, err)
			}
		}
	}
	var retainedHeld bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT s.status='open' AND t.status='queued' AND t.started_at IS NULL
	 AND (SELECT count(*)=2 FROM session_controls c WHERE c.environment_id=s.environment_id AND c.session_id=s.id AND c.kind='interrupt')
	 AND (SELECT count(*)=2 FROM session_holds h WHERE h.environment_id=s.environment_id AND h.session_id=s.id)
	 AND (SELECT count(*)=1 FROM session_holds h WHERE h.environment_id=s.environment_id AND h.session_id=s.id AND h.released_at IS NULL)
	 FROM sessions s JOIN turns t ON(t.environment_id,t.session_id)=(s.environment_id,s.id) WHERE s.id=$1 AND t.id=$2`, toInterrupt.SessionID, toInterrupt.Turn.TurnID).Scan(&retainedHeld); err != nil || !retainedHeld {
		t.Fatalf("interrupt discarded work or duplicated holds: %v %v", retainedHeld, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET revoked_at=clock_timestamp() WHERE token_hash=$1`, auth.HashAPIKey(interrupter))
	call(keyInterruptPath, interrupter, interruptBody, http.StatusUnauthorized)

	// Closing rejects new input but retains already admitted Turns. Both external
	// transports use current close authority and durable, caller-scoped receipts.
	toClose := decode(call(start, owner, `{"input":[{"type":"text","text":"1"}],"idempotency_key":"close-target"}`, http.StatusOK))
	closePath := base + "/sessions/" + toClose.SessionID + "/close"
	keyClosePath := "/v1/sessions/" + toClose.SessionID + "/close"
	closeBody := `{"idempotency_key":"close-retained"}`
	closer := issue(auth.PermissionSessionsClose)
	otherCloser, err := issueAPIKeyWithRole(t, t.Context(), f.Pool, issuer, otherScope, testAPIKeyInput{Name: "other close", RolePermissions: []auth.Permission{auth.PermissionSessionsClose}})
	if err != nil {
		t.Fatal(err)
	}
	call(closePath, "", closeBody, http.StatusUnauthorized)
	call(closePath, viewer, closeBody, http.StatusForbidden)
	call(keyClosePath, starter, closeBody, http.StatusForbidden)
	call(keyClosePath, otherCloser.Raw, closeBody, http.StatusForbidden)
	call(fmt.Sprintf("/api/projects/project/environments/other/sessions/%s/close", toClose.SessionID), owner, closeBody, http.StatusForbidden)
	for _, body := range []string{`{}`, `{"idempotency_key":"close","caller_id":"fake"}`} {
		call(closePath, owner, body, http.StatusBadRequest)
	}
	for _, transport := range []struct{ path, token string }{{closePath, owner}, {keyClosePath, closer}} {
		var receipt, replay api.SessionCloseReceipt
		if err := json.Unmarshal(call(transport.path, transport.token, closeBody, http.StatusOK), &receipt); err != nil || receipt.ID == "" || receipt.SessionID != toClose.SessionID || receipt.Status != "accepted" {
			t.Fatalf("close receipt: %+v %v", receipt, err)
		}
		if err := json.Unmarshal(call(transport.path, transport.token, closeBody, http.StatusOK), &replay); err != nil || replay != receipt {
			t.Fatalf("close retry: %+v %v", replay, err)
		}
	}
	var retainedClosing bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT s.status='closing' AND t.status='queued' AND (SELECT count(*)=2 FROM session_controls c WHERE c.environment_id=s.environment_id AND c.session_id=s.id AND c.kind='close') FROM sessions s JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id) WHERE s.id=$1 AND t.id=$2`, toClose.SessionID, toClose.Turn.TurnID).Scan(&retainedClosing); err != nil || !retainedClosing {
		t.Fatalf("close discarded work or duplicated controls: %v %v", retainedClosing, err)
	}
	expectCode(call(base+"/sessions/"+toClose.SessionID+"/enqueue", owner, `{"input":[{"type":"text","text":"2"}]}`, http.StatusConflict), "admission_unavailable")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET revoked_at=clock_timestamp() WHERE token_hash=$1`, auth.HashAPIKey(closer))
	call(keyClosePath, closer, closeBody, http.StatusUnauthorized)

	sdkKey, err := issueAPIKeyWithRole(t, t.Context(), f.Pool, issuer, scope, testAPIKeyInput{Name: "SDK composition", RolePermissions: []auth.Permission{auth.PermissionAgentsUse, auth.PermissionSessionsRead, auth.PermissionSessionsSend, auth.PermissionSessionsCreateKeyed, auth.PermissionSessionsResume, auth.PermissionSessionsInterrupt, auth.PermissionSessionsClose, auth.PermissionSessionsCancel}})
	if err != nil {
		t.Fatal(err)
	}
	sdkServer := httptest.NewServer(handler)
	defer sdkServer.Close()
	assertAgentCLIHTTP(t, sdkServer.URL, sdkKey.Raw, httpFixture.cliSession(t, f.User, orgID), "project", "test")
	script, err := filepath.Abs("../../sdk/typescript/testdata/retained-work-http.ts")
	if err != nil {
		t.Fatal(err)
	}
	inputConfig, err := json.Marshal(map[string]string{"url": sdkServer.URL, "apiKey": sdkKey.Raw, "sessionId": keyAdmission.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	sdkCommand := exec.CommandContext(t.Context(), "bun", script)
	sdkCommand.Stdin = bytes.NewReader(inputConfig)
	if output, err := sdkCommand.CombinedOutput(); err != nil {
		t.Fatalf("SDK HTTP composition: %v\n%s", err, output)
	}
	t.Run("exact Turn interrupt across public clients", func(t *testing.T) {
		a, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.UserCaller(f.Principal(t)), session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "exact", Input: json.RawMessage(`[{"type":"text","text":"run"}]`)})
		if err != nil {
			t.Fatal(err)
		}
		path := "/v1/sessions/" + f.Session.String() + "/turns/" + a.TurnID.String() + "/interrupt"
		body := `{"idempotency_key":"exact-stop"}`
		call(path, starter, body, http.StatusForbidden)
		cookiePath := base + "/sessions/" + f.Session.String() + "/turns/" + a.TurnID.String() + "/interrupt"
		call(cookiePath, viewer, body, http.StatusForbidden)
		for _, transport := range []struct{ path, token string }{{path, sdkKey.Raw}, {cookiePath, owner}} {
			raw := call(transport.path, transport.token, body, http.StatusConflict)
			expectCode(raw, "turn_not_interruptible")
			var rejected api.HTTPErrorResponse
			if err := json.Unmarshal(raw, &rejected); err != nil || string(rejected.Error.Details["status"]) != `"queued"` || string(rejected.Error.Details["reason"]) != `"not_started"` {
				t.Fatalf("queued rejection details: %s %v", raw, err)
			}
		}
		e := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1, AuthenticatedHostVersion: 1}
		if _, err := dispatchRuntimeTurn(t, f.Pool, e); err != nil {
			t.Fatal(err)
		}
		var receipt api.TurnInterruptReceipt
		if err := json.Unmarshal(call(path, sdkKey.Raw, body, http.StatusOK), &receipt); err != nil || receipt.TurnID != a.TurnID.String() || receipt.ID == "" {
			t.Fatalf("exact receipt: %+v %v", receipt, err)
		}
		run := newAgentTestCLI(t, sdkServer.URL, sdkKey.Raw)
		var replay api.TurnInterruptReceipt
		if err := json.Unmarshal(run("session", "turn", "interrupt", a.TurnID.String(), "--session", f.Session.String(), "--idempotency-key", "exact-stop", "--json"), &replay); err != nil || replay != receipt {
			t.Fatalf("CLI exact replay: %+v %v", replay, err)
		}
		script, err := filepath.Abs("../../sdk/typescript/testdata/turn-interrupt-http.ts")
		if err != nil {
			t.Fatal(err)
		}
		config, _ := json.Marshal(map[string]string{"url": sdkServer.URL, "apiKey": sdkKey.Raw, "sessionId": f.Session.String(), "turnId": a.TurnID.String(), "receiptId": receipt.ID})
		command := exec.CommandContext(t.Context(), "bun", script)
		command.Stdin = bytes.NewReader(config)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("SDK exact interrupt: %v\n%s", err, output)
		}
		var unchanged bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM session_holds WHERE session_id=$1) AND (SELECT status='ready' FROM session_processes WHERE session_id=$1)`, f.Session).Scan(&unchanged); err != nil || !unchanged {
			t.Fatalf("exact interrupt changed hold/process: %v %v", unchanged, err)
		}
	})
	expectCode(call(start, owner, `{"input":[{"type":"json","value":null}]}`, http.StatusUnprocessableEntity), "content_kind_unsupported")
	expectCode(call(start, owner, `{"input":{"type":"message","content":"shorthand"}}`, http.StatusBadRequest), "bad_request")
	var conversational api.CreateSessionResponse
	if err := json.Unmarshal(call(start, owner, `{"input":[{"type":"text","text":"a\u0000雪"}],"idempotency_key":"message-nul"}`, http.StatusOK), &conversational); err != nil {
		t.Fatal(err)
	}
	messageRead := httpFixture.request(t, http.MethodGet, base+"/sessions/"+conversational.SessionID+"/turns/"+conversational.Turn.TurnID, owner, "")
	var messageTurn api.AgentTurn
	if messageRead.Code != http.StatusOK || json.Unmarshal(messageRead.Body.Bytes(), &messageTurn) != nil || string(messageTurn.Input) != `[{"text":"a\u0000雪","type":"text"}]` {
		t.Fatalf("HTTP message bytes changed: %s", messageRead.Body.String())
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET revoked_at=clock_timestamp() WHERE token_hash=$1`, auth.HashAPIKey(starter))
	call("/v1/sessions", starter, `{"input":[{"type":"text","text":"5"}],"idempotency_key":"key-start"}`, http.StatusUnauthorized)
	// A revoked Computer cannot run retained input, but current control authority
	// can still cancel it over either external transport.
	var retainedComputer uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM sessions WHERE id=$1`, first.SessionID).Scan(&retainedComputer); err != nil {
		t.Fatal(err)
	}
	f.PinRevokedImage(t, retainedComputer)
	reader := issue(auth.PermissionSessionsRead)
	for _, transport := range []struct{ path, token string }{{base + "/sessions/" + first.SessionID, viewer}, {"/v1/sessions/" + first.SessionID, reader}} {
		response := httpFixture.request(t, http.MethodGet, transport.path, transport.token, "")
		var retained api.AgentSession
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &retained) != nil || retained.ID != first.SessionID || retained.Status != "open" {
			t.Fatalf("retained session: %d %s", response.Code, response.Body.String())
		}
		response = httpFixture.request(t, http.MethodGet, transport.path+"/turns?limit=1", transport.token, "")
		var page api.AgentTurnsPage
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Turns) != 1 || page.Turns[0].ID != first.Turn.TurnID || page.Turns[0].Status != "queued" || page.NextCursor != "1" {
			t.Fatalf("retained queue: %d %s", response.Code, response.Body.String())
		}
		response = httpFixture.request(t, http.MethodGet, transport.path+"/turns/"+first.Turn.TurnID, transport.token, "")
		var turn api.AgentTurn
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &turn) != nil || turn.ID != first.Turn.TurnID || string(turn.Input) != `[{"text":"{\"work\":1}","type":"text"}]` {
			t.Fatalf("retained input: %d %s", response.Code, response.Body.String())
		}
	}
	if response := httpFixture.request(t, http.MethodGet, "/v1/sessions/"+first.SessionID, sender, ""); response.Code != http.StatusForbidden {
		t.Fatalf("send permission allowed reading: %d", response.Code)
	}

	cancelPath := base + "/sessions/" + first.SessionID + "/cancel"
	cancelBody := `{"idempotency_key":"cancel-retained"}`
	call(cancelPath, "", cancelBody, http.StatusUnauthorized)
	call(cancelPath, viewer, cancelBody, http.StatusForbidden)
	call(cancelPath, owner, `{}`, http.StatusBadRequest)
	call(cancelPath, owner, `{"idempotency_key":"cancel-retained","caller_id":"fake"}`, http.StatusBadRequest)
	call("/v1/sessions/"+first.SessionID+"/cancel", issue(auth.PermissionAgentsUse), cancelBody, http.StatusForbidden)
	call(fmt.Sprintf("/api/projects/project/environments/other/sessions/%s/cancel", first.SessionID), owner, cancelBody, http.StatusForbidden)
	var cancellation api.SessionCancelReceipt
	if err := json.Unmarshal(call(cancelPath, owner, cancelBody, http.StatusOK), &cancellation); err != nil || cancellation.ID == "" || cancellation.SessionID != first.SessionID || cancellation.Status != "accepted" {
		t.Fatalf("cancel receipt: %+v %v", cancellation, err)
	}
	var replay api.SessionCancelReceipt
	if err := json.Unmarshal(call(cancelPath, owner, cancelBody, http.StatusOK), &replay); err != nil || replay != cancellation {
		t.Fatalf("cancel replay: %+v %v", replay, err)
	}
	var unsettled int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE session_id=$1 AND status<>'cancelled'`, first.SessionID).Scan(&unsettled); err != nil || unsettled != 0 {
		t.Fatalf("retained input not cancelled: %d %v", unsettled, err)
	}
	canceller := issue(auth.PermissionSessionsCancel)
	call("/v1/sessions/"+keyAdmission.SessionID+"/cancel", canceller, cancelBody, http.StatusOK)
	call("/v1/sessions/"+keyAdmission.SessionID+"/cancel", canceller, cancelBody, http.StatusOK)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET revoked_at=clock_timestamp() WHERE token_hash=$1`, auth.HashAPIKey(canceller))
	call("/v1/sessions/"+keyAdmission.SessionID+"/cancel", canceller, cancelBody, http.StatusUnauthorized)

	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET status='closed' WHERE id=$1`, first.SessionID)
	expectCode(call(enqueue, owner, `{"input":[{"type":"text","text":"6"}]}`, http.StatusConflict), "admission_unavailable")

}

func TestAgentHTTPTransientFailure(t *testing.T) {
	server := &Server{log: discardTestLogger()}
	response := httptest.NewRecorder()
	server.writeAgentHTTPError(response, httptest.NewRequest(http.MethodPost, "/v1/sessions", nil), errors.New("connection lost with private details"))
	var body api.HTTPErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusServiceUnavailable || body.Error.Code != "service_unavailable" || body.Error.Message == "connection lost with private details" {
		t.Fatalf("transient failure: %d %s (%v)", response.Code, response.Body.String(), err)
	}
}
