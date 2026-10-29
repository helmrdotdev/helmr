package controlplane

import (
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func runtimeTestSession(f sessiontest.Fixture) workerapi.RuntimeSession {
	return workerapi.RuntimeSession{EnvironmentID: f.Environment.String(), SessionID: f.Session.String(), ProcessEpoch: 1, ComputerLeaseEpoch: 1}
}
func runtimeEnqueue(f sessiontest.Fixture, key string) workerapi.AgentOperationRequest {
	payload, _ := json.Marshal(map[string]any{"tool": "enqueue", "arguments": map[string]any{"sessionId": f.Session.String(), "input": json.RawMessage(`[{"type":"text","text":"hello"}]`), "idempotencyKey": key}})
	return workerapi.AgentOperationRequest{MCPScopeID: f.MCPScope.String(), Session: runtimeTestSession(f), RequestID: uuid.New().String(), AuthorityGeneration: 1, Method: workerapi.AgentMethodRuntimeMCP, Payload: payload}
}

func TestWorkerAgentClientAuthorityAndRetry(t *testing.T) {
	f := sessiontest.New(t)
	handler := newPostgresServer(t, f.Pool)
	secret := seedHostSecret(t, f.Pool, f.Worker)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := secret.client(t, server.URL)
	grant, err := client.RenewAgentAuthority(t.Context(), runtimeTestSession(f))
	if err != nil || grant.AuthorityGeneration != 1 || !grant.ExpiresAt.After(time.Now()) || grant.ExpiresAt.After(time.Now().Add(31*time.Second)) {
		t.Fatalf("grant: %+v %v", grant, err)
	}
	request := runtimeEnqueue(f, "stable-key")
	first, err := client.AgentOperation(t.Context(), request)
	if err != nil || first.Error != nil || !json.Valid(first.Value) {
		t.Fatalf("first: %+v %v", first, err)
	}
	request.RequestID = uuid.New().String()
	retry, err := client.AgentOperation(t.Context(), request)
	if err != nil || retry.Error != nil || string(retry.Value) != string(first.Value) {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), "SELECT count(*) FROM turns").Scan(&count); err != nil || count != 1 {
		t.Fatalf("turns=%d err=%v", count, err)
	}
}

func TestWorkerAgentAdmissionFences(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		status    int
		code      string
	}{
		{"old generation", "UPDATE sessions SET authority_generation=2", 200, "authority_changed"},
		{"held", "INSERT INTO session_holds(environment_id,id,session_id,scope,reason) SELECT environment_id,gen_random_uuid(),id,'local','test' FROM sessions", 200, "authority_changed"},
		{"expired lease", "UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'", 200, "authority_changed"},
		{"replaced host epoch", "UPDATE worker_hosts SET current_epoch=2", 401, ""},
		{"revoked host claims", "UPDATE worker_hosts SET claim_version=2", 401, ""},
		{"revoked group claims", "UPDATE worker_groups SET claim_version=2", 401, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := sessiontest.New(t)
			handler := newPostgresServer(t, f.Pool)
			client := newWorkerHTTPClient(t, handler, f.Pool, f.Worker)
			dbtest.MustExec(t, t.Context(), f.Pool, test.sql)
			var response workerapi.AgentOperationResponse
			client.post(t, "/worker/v1/sessions/operations", runtimeEnqueue(f, "key"), test.status, &response)
			if test.code != "" && (response.Error == nil || response.Error.Code != test.code) {
				t.Fatalf("response: %+v", response)
			}
			if test.code == "authority_changed" {
				var captured bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action='runtime.operation_refused' AND org_id IS NULL AND actor_worker_host_id IS NOT NULL AND credential_id=$1 AND details->>'reason'='stale_authority' AND details->>'operation'='POST /worker/v1/sessions/operations')`, f.Worker).Scan(&captured); err != nil || !captured {
					t.Fatalf("missing verified refusal: %v %v", captured, err)
				}
			}
			var count int
			if err := f.Pool.QueryRow(t.Context(), "SELECT count(*) FROM turns").Scan(&count); err != nil || count != 0 {
				t.Fatalf("turns=%d err=%v", count, err)
			}
		})
	}
}

func TestWorkerAgentAuthorityDoesNotReleaseHold(t *testing.T) {
	f := sessiontest.New(t)
	handler := newPostgresServer(t, f.Pool)
	client := newWorkerHTTPClient(t, handler, f.Pool, f.Worker)
	dbtest.MustExec(t, t.Context(), f.Pool, "UPDATE sessions SET authority_generation=2")
	dbtest.MustExec(t, t.Context(), f.Pool, "UPDATE session_processes SET status='starting'")
	dbtest.MustExec(t, t.Context(), f.Pool, "INSERT INTO session_holds(environment_id,id,session_id,scope,reason) SELECT environment_id,gen_random_uuid(),id,'local','test' FROM sessions")
	var grant workerapi.AgentAuthorityResponse
	client.post(t, "/worker/v1/sessions/authority", runtimeTestSession(f), http.StatusOK, &grant)
	if grant.AuthorityGeneration != 2 {
		t.Fatalf("grant: %+v", grant)
	}
	request := runtimeEnqueue(f, "held")
	request.AuthorityGeneration = 2
	var response workerapi.AgentOperationResponse
	client.post(t, "/worker/v1/sessions/operations", request, 200, &response)
	if response.Error == nil || response.Error.Code != "authority_changed" {
		t.Fatalf("response: %+v", response)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, "UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'")
	client.post(t, "/worker/v1/sessions/authority", runtimeTestSession(f), http.StatusConflict, nil)
}

func TestWorkerAgentRejectsCallerSubstitution(t *testing.T) {
	f := sessiontest.New(t)
	handler := newPostgresServer(t, f.Pool)
	client := newWorkerHTTPClient(t, handler, f.Pool, f.Worker)
	local := client
	local.hostCredential = "guest-local-mcp-bearer"
	local.post(t, "/worker/v1/sessions/authority", runtimeTestSession(f), http.StatusUnauthorized, nil)
	request := runtimeEnqueue(f, "")
	var invalid workerapi.AgentOperationResponse
	client.post(t, "/worker/v1/sessions/operations", request, http.StatusOK, &invalid)
	if invalid.Error == nil || invalid.Error.Code != "invalid_arguments" {
		t.Fatalf("invalid input did not settle: %+v", invalid)
	}
	request = runtimeEnqueue(f, "key")
	request.TurnID = uuid.New().String()
	invalid = workerapi.AgentOperationResponse{}
	client.post(t, "/worker/v1/sessions/operations", request, http.StatusOK, &invalid)
	if invalid.Error == nil || invalid.Error.Code != "invalid_arguments" {
		t.Fatalf("Turn substitution did not settle: %+v", invalid)
	}
	request = runtimeEnqueue(f, "key")
	request.Session.ProcessEpoch = 2
	var response workerapi.AgentOperationResponse
	client.post(t, "/worker/v1/sessions/operations", request, http.StatusOK, &response)
	if response.Error == nil || response.Error.Code != "authority_changed" {
		t.Fatalf("response: %+v", response)
	}
}

func TestWorkerAgentCommittedReceiptSurvivesAdmissionRevocation(t *testing.T) {
	for _, change := range []string{"hold", "generation", "origin-ended"} {
		t.Run(change, func(t *testing.T) {
			f := sessiontest.New(t)
			handler := newPostgresServer(t, f.Pool)
			client := newWorkerHTTPClient(t, handler, f.Pool, f.Worker)
			origin, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.UserCaller(f.Principal(t)), session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "origin", Input: json.RawMessage(`[{"type":"text","text":"1"}]`)})
			if err != nil {
				t.Fatal(err)
			}
			execution := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, WorkerHostID: f.Worker, WorkerEpoch: 1, ProcessEpoch: 1, LeaseEpoch: 1, AuthorityGeneration: 1, AuthenticatedHostVersion: 1}
			if _, err = dispatchRuntimeTurn(t, f.Pool, execution); err != nil {
				t.Fatal(err)
			}
			request := runtimeEnqueue(f, strings.Repeat("x", 512))
			var first workerapi.AgentOperationResponse
			client.post(t, "/worker/v1/sessions/operations", request, 200, &first)
			if first.Error != nil {
				t.Fatalf("first: %+v", first.Error)
			}
			switch change {
			case "hold":
				dbtest.MustExec(t, t.Context(), f.Pool, "INSERT INTO session_holds(environment_id,id,session_id,scope,reason) SELECT environment_id,gen_random_uuid(),id,'local','test' FROM sessions")
			case "generation":
				dbtest.MustExec(t, t.Context(), f.Pool, "UPDATE sessions SET authority_generation=2")
			case "origin-ended":
				if err = closeRuntimeProcessing(t, f.Pool, execution, origin.TurnID); err != nil {
					t.Fatal(err)
				}
			}
			if change == "generation" {
				var stale workerapi.AgentOperationResponse
				client.post(t, "/worker/v1/sessions/operations", request, 200, &stale)
				if stale.Error == nil || stale.Error.Code != "authority_changed" {
					t.Fatalf("stale receipt: %+v", stale)
				}
				var captured bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action='runtime.operation_refused' AND actor_worker_host_id=$1 AND actor_session_id IS NULL AND org_id IS NULL AND authentication_result='verified' AND authorization_evidence->>'decision'='denied' AND authorization_evidence->'checks' @> '[{"type":"runtime_binding","result":"failed","reason":"generation_changed","generation":1,"observed_generation":2}]'::jsonb)`, f.Worker).Scan(&captured); err != nil || !captured {
					t.Fatal("runtime HTTP refusal lost check", captured, err)
				}
				// A new observation with renewed authority can recover the same
				// key; the stale envelope never borrows the new generation.
				request.AuthorityGeneration = 2
			}
			var retry workerapi.AgentOperationResponse
			client.post(t, "/worker/v1/sessions/operations", request, 200, &retry)
			if retry.Error != nil || string(retry.Value) != string(first.Value) {
				t.Fatalf("receipt changed: %+v error=%+v", retry, retry.Error)
			}
			if change == "generation" {
				request.AuthorityGeneration = 1
			}
			if change == "origin-ended" {
				return
			} // Session MCP remains authorized after a Turn.
			next := runtimeEnqueue(f, "new-decision")
			request.Payload = next.Payload
			request.RequestID = "new-request"
			var denied workerapi.AgentOperationResponse
			client.post(t, "/worker/v1/sessions/operations", request, 200, &denied)
			if denied.Error == nil || denied.Error.Code != "authority_changed" {
				t.Fatalf("new work admitted: %+v", denied)
			}
		})
	}
}

func TestWorkerAgentAcceptedFrameDoesNotExpandBeyondHTTPBound(t *testing.T) {
	f := sessiontest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	request := runtimeEnqueue(f, "large")
	request.Payload = []byte(`{"tool":"enqueue","arguments":{"sessionId":"` + f.Session.String() + `","input":[{"type":"text","text":"` + strings.Repeat("<", 32<<10) + `"}],"idempotencyKey":"large"}}`)
	result, err := client.AgentOperation(t.Context(), request)
	if err != nil || result.Error != nil || len(result.Value) == 0 {
		t.Fatalf("accepted native frame did not settle: %+v %v", result.Error, err)
	}
	var stored []byte
	if err = f.Pool.QueryRow(t.Context(), "SELECT input FROM turns").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var value []struct {
		Text string `json:"text"`
	}
	if err = json.Unmarshal(stored, &value); err != nil || len(value) != 1 || len(value[0].Text) != 32<<10 {
		t.Fatalf("input changed: %v", err)
	}
}

func TestWorkerAgentAttachmentSurvivesClientReplacement(t *testing.T) {
	f := sessiontest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	secret := seedHostSecret(t, f.Pool, f.Worker)
	for sequence := sessiontest.SeededAttachment + 1; sequence <= sessiontest.SeededAttachment+2; sequence++ {
		client := secret.client(t, server.URL)
		result, err := client.AcquireAgentAttachment(t.Context(), runtimeTestSession(f))
		if err != nil || result.AttachmentSequence != sequence || result.AuthorityGeneration != 1 || !result.ExpiresAt.After(time.Now()) {
			t.Fatalf("attachment %d: %+v %v", sequence, result, err)
		}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, "UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'")
	_, err := secret.client(t, server.URL).AcquireAgentAttachment(t.Context(), runtimeTestSession(f))
	var denied interface{ SessionAuthorityRejected() bool }
	if !errors.As(err, &denied) || !denied.SessionAuthorityRejected() {
		t.Fatalf("expired attachment not rejected: %v", err)
	}
}

func acknowledgeTestAgentControl(t *testing.T, client *workerclient.Client, sessionID workerapi.RuntimeSession, attachment int64) {
	t.Helper()
	control, err := client.PrepareAgentControl(t.Context(), workerapi.AgentControlRequest{Session: sessionID, AttachmentSequence: attachment})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AcknowledgeAgentControl(t.Context(), workerapi.AgentControlReceipt{Session: sessionID, AttachmentSequence: attachment, Sequence: control.Sequence, AuthorityGeneration: control.AuthorityGeneration, Kind: control.Kind}); err != nil {
		t.Fatal(err)
	}
}

func fixtureMCPScope(t *testing.T, f sessiontest.Fixture) uuid.UUID {
	t.Helper()
	host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	e := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1, AuthenticatedHostVersion: 1}
	scope, err := session.IssueMCPScope(t.Context(), f.Pool, session.Caller{Kind: "session", ID: f.Session, Execution: e, Host: &host}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return scope.ID
}
