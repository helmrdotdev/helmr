package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func runtimeAdmissionOrigin(t *testing.T) (sessiontest.Fixture, session.Admission, session.Execution) {
	t.Helper()
	f := sessiontest.New(t)
	sessiontest.ObserveProvisioning(t, f)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE image_specs SET seed=jsonb_build_object('profile',$2::text) WHERE environment_id=$1;
 UPDATE template_definitions SET cpu_millis=1000,memory_bytes=536870912 WHERE environment_id=$1;
 UPDATE computers SET image_spec_id=$3,origin_deployment_id=$3,origin_template_id=origin_deployment_id,cpu_millis=1000,memory_bytes=536870912,storage_reservation_bytes=$4 WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, f.Environment, definition.ComputerSeedProfile, f.Deployment, disk.SeedCapacity)
	origin, err := session.Enqueue(t.Context(), f.Pool, nil, session.Caller{Kind: "user", ID: f.User}, session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "origin", Input: json.RawMessage(`[{"type":"text","text":"1"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	execution := session.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	if _, err = dispatchRuntimeTurn(t, f.Pool, execution); err != nil {
		t.Fatal(err)
	}
	return f, origin, execution
}

func TestWorkerAgentStartSpawnReconcileLostResponse(t *testing.T) {
	for _, method := range []workerapi.AgentOperationMethod{workerapi.AgentMethodStart, workerapi.AgentMethodSpawn} {
		for _, placed := range []bool{false, true} {
			name := string(method) + "/fresh"
			if placed {
				name = string(method) + "/placed"
			}
			t.Run(name, func(t *testing.T) {
				f, origin, execution := runtimeAdmissionOrigin(t)
				handler := newPostgresServer(t, f.Pool)
				var lose atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/worker/v1/sessions/operations" && lose.CompareAndSwap(true, false) {
						// Complete the real transaction, then lose the entire HTTP receipt.
						committed := httptest.NewRecorder()
						handler.ServeHTTP(committed, r)
						if committed.Code != http.StatusOK {
							t.Errorf("commit status=%d body=%s", committed.Code, committed.Body.String())
						}
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = connection.Close()
						return
					}
					handler.ServeHTTP(w, r)
				}))
				defer server.Close()
				secret := seedHostSecret(t, f.Pool, f.Worker)
				client := secret.client(t, server.URL)
				payload := map[string]any{"agent": "agent", "input": json.RawMessage(`[{"type":"text","text":"work"}]`), "idempotencyKey": "retained-decision"}
				if placed {
					payload["computerId"] = f.Computer.String()
				}
				tool := strings.ToLower(string(method))
				body, _ := json.Marshal(map[string]any{"tool": tool, "arguments": payload})
				request := workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: "retained-request", AuthorityGeneration: 1, Method: workerapi.AgentMethodRuntimeMCP, Payload: body}
				// Spawn remains pinned even when no current Deployment is selected.
				if method == workerapi.AgentMethodSpawn {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.Environment)
				}
				lose.Store(true)
				if response, err := client.AgentOperation(t.Context(), request); err == nil {
					t.Fatalf("lost reply unexpectedly received: %+v", response)
				}
				first, err := secret.client(t, server.URL).AgentOperation(t.Context(), request)
				if err != nil || first.Error != nil {
					t.Fatalf("retry: %+v %v", first, err)
				}
				var receipt struct {
					SessionID, TurnID string
					Sequence          int64
					Created           bool
				}
				if err = json.Unmarshal(first.Value, &receipt); err != nil || receipt.Sequence != 1 {
					t.Fatalf("receipt=%s err=%v", first.Value, err)
				}
				var valid bool
				err = f.Pool.QueryRow(t.Context(), `SELECT s.deployment_id=$3 AND s.requester_session_id=$4 AND s.origin_turn_id IS NULL
 AND s.causal_depth=1
 AND CASE WHEN $5 THEN s.parent_session_id=$4 AND s.root_session_id=$4 ELSE s.parent_session_id IS NULL AND s.root_session_id=s.id END
 AND CASE WHEN $6 THEN s.computer_id=$7 ELSE s.computer_id<>$7 AND c.image_build_id IS NOT NULL END
 AND (SELECT count(*) FROM turns WHERE environment_id=$1)=2
 FROM sessions s JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id) WHERE s.environment_id=$1 AND s.id=$2`, f.Environment, receipt.SessionID, f.Deployment, f.Session, method == workerapi.AgentMethodSpawn, placed, f.Computer).Scan(&valid)
				if err != nil || !valid {
					t.Fatalf("admission ownership/placement/count: %v %v", valid, err)
				}
				if err = closeRuntimeProcessing(t, f.Pool, execution, origin.TurnID); err != nil {
					t.Fatal(err)
				}
				replay, err := client.AgentOperation(t.Context(), request)
				if err != nil || replay.Error != nil || string(replay.Value) != string(first.Value) {
					t.Fatalf("closed-origin receipt: %+v %v", replay, err)
				}
				payload["input"] = json.RawMessage(`[{"type":"text","text":"changed"}]`)
				request.Payload, _ = json.Marshal(map[string]any{"tool": tool, "arguments": payload})
				conflict, err := client.AgentOperation(t.Context(), request)
				if err != nil || conflict.Error == nil || conflict.Error.Code != "idempotency_conflict" {
					t.Fatalf("changed retry: %+v %v", conflict, err)
				}
			})
		}
	}
}

func TestWorkerAgentStartSpawnRejectCallerAndPayloadSubstitution(t *testing.T) {
	f, _, _ := runtimeAdmissionOrigin(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	for _, method := range []workerapi.AgentOperationMethod{workerapi.AgentMethodStart, workerapi.AgentMethodSpawn} {
		for _, test := range []struct{ name, payload, turn, code string }{
			{"missing key", `{"agent":"agent","input":[{"type":"text","text":"work"}]}`, "", "invalid_arguments"},
			{"foreign origin", `{"agent":"agent","input":[{"type":"text","text":"work"}]}`, uuid.NewV7().String(), "invalid_arguments"},
			{"bad placement", `{"agent":"agent","input":[{"type":"text","text":"work"}],"computerId":""}`, "", "invalid_arguments"},
			{"null placement", `{"agent":"agent","input":[{"type":"text","text":"work"}],"computerId":null}`, "", "invalid_arguments"},
			{"numeric placement", `{"agent":"agent","input":[{"type":"text","text":"work"}],"computerId":1}`, "", "invalid_arguments"},
			{"missing input", `{"agent":"agent"}`, "", "invalid_arguments"},
			{"deployment override", `{"agent":"agent","input":[{"type":"text","text":"work"}],"deploymentId":"other"}`, "", "invalid_arguments"},
			{"environment override", `{"agent":"agent","input":[{"type":"text","text":"work"}],"environmentId":"other"}`, "", "invalid_arguments"},
			{"conversation key", `{"agent":"agent","input":[{"type":"text","text":"work"}],"sessionKey":"shared"}`, "", "invalid_arguments"},
		} {
			t.Run(string(method)+"/"+test.name, func(t *testing.T) {
				var arguments map[string]any
				json.Unmarshal([]byte(test.payload), &arguments)
				if test.name != "missing key" {
					arguments["idempotencyKey"] = "decision"
				}
				body, _ := json.Marshal(map[string]any{"tool": strings.ToLower(string(method)), "arguments": arguments})
				response, err := client.AgentOperation(t.Context(), workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: uuid.NewV7().String(), AuthorityGeneration: 1, TurnID: test.turn, Method: workerapi.AgentMethodRuntimeMCP, Payload: body})
				if err != nil || response.Error == nil || response.Error.Code != test.code {
					t.Fatalf("rejection=%+v err=%v", response, err)
				}
			})
		}
	}
	var sessions, computers, preparations, turns int
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM sessions),(SELECT count(*) FROM computers),(SELECT count(*) FROM image_builds),(SELECT count(*) FROM turns)`).Scan(&sessions, &computers, &preparations, &turns); err != nil || sessions != 1 || computers != 1 || preparations != 0 || turns != 1 {
		t.Fatalf("unauthorized admission/reservation: sessions=%d computers=%d preparations=%d turns=%d err=%v", sessions, computers, preparations, turns, err)
	}
}

func TestWorkerAgentMCPCreationUsesSessionAuthorityAndStableKey(t *testing.T) {
	for _, tool := range []string{"spawn", "start"} {
		t.Run(tool, func(t *testing.T) {
			f, origin, execution := runtimeAdmissionOrigin(t)
			server := httptest.NewServer(newPostgresServer(t, f.Pool))
			defer server.Close()
			client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
			arguments := map[string]any{"agent": "agent", "input": json.RawMessage(`[{"type":"text","text":"work"}]`), "idempotencyKey": "durable-model-decision"}
			payload := func() json.RawMessage {
				b, _ := json.Marshal(map[string]any{"tool": tool, "arguments": arguments})
				return b
			}
			request := workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: "first-rpc", AuthorityGeneration: 1, Method: workerapi.AgentMethodRuntimeMCP, Payload: payload()}
			first, err := client.AgentOperation(t.Context(), request)
			if err != nil || first.Error != nil {
				t.Fatalf("first %+v %v", first, err)
			}
			var receipt struct{ SessionID, TurnID string }
			if err := json.Unmarshal(first.Value, &receipt); err != nil {
				t.Fatal(err)
			}
			var valid bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT s.requester_session_id=$3 AND s.origin_turn_id IS NULL AND t.origin_turn_id IS NULL AND t.caller_id=$3
                AND CASE WHEN $4='spawn' THEN s.parent_session_id=$3 ELSE s.parent_session_id IS NULL END
                FROM sessions s JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id) WHERE s.environment_id=$1 AND s.id=$2`, f.Environment, receipt.SessionID, f.Session, tool).Scan(&valid); err != nil || !valid {
				t.Fatalf("origin %v %v", valid, err)
			}
			if err := closeRuntimeProcessing(t, f.Pool, execution, origin.TurnID); err != nil {
				t.Fatal(err)
			}
			request.RequestID = "reconnect-rpc"
			retry, err := client.AgentOperation(t.Context(), request)
			if err != nil || retry.Error != nil || string(retry.Value) != string(first.Value) {
				t.Fatalf("retry %+v %v", retry, err)
			}
			arguments["idempotencyKey"] = "after-turn"
			request.Payload = payload()
			next, err := client.AgentOperation(t.Context(), request)
			if err != nil || next.Error != nil {
				t.Fatalf("Session capability after Turn %+v %v", next, err)
			}
			delete(arguments, "idempotencyKey")
			request.Payload = payload()
			rejected, err := client.AgentOperation(t.Context(), request)
			if err != nil || rejected.Error == nil || rejected.Error.Code != "invalid_arguments" {
				t.Fatalf("missing key %+v %v", rejected, err)
			}
			arguments["idempotencyKey"] = "durable-model-decision"
			request.Payload = payload()
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET authority_generation=2 WHERE id=$1`, f.Session)
			stale, err := client.AgentOperation(t.Context(), request)
			if err != nil || stale.Error == nil || stale.Error.Code != "authority_changed" {
				t.Fatalf("stale receipt %+v %v", stale, err)
			}
			request.AuthorityGeneration = 2
			recovered, err := client.AgentOperation(t.Context(), request)
			if err != nil || recovered.Error != nil || string(recovered.Value) != string(first.Value) {
				t.Fatalf("renewed receipt %+v %v", recovered, err)
			}
			request.AuthorityGeneration = 1
			arguments["idempotencyKey"] = "after-stop"
			request.Payload = payload()
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET authority_generation=2 WHERE id=$1`, f.Session)
			rejected, err = client.AgentOperation(t.Context(), request)
			if err != nil || rejected.Error == nil || rejected.Error.Code != "authority_changed" {
				t.Fatalf("stale Session %+v %v", rejected, err)
			}
		})
	}
}

func TestWorkerAgentRequiresToolEnvelopeForSessionOperations(t *testing.T) {
	f, origin, _ := runtimeAdmissionOrigin(t)
	client := newWorkerHTTPClient(t, newPostgresServer(t, f.Pool), f.Pool, f.Worker)
	for _, method := range []workerapi.AgentOperationMethod{workerapi.AgentMethodStart, workerapi.AgentMethodSpawn, workerapi.AgentMethodEnqueue, workerapi.AgentMethodSendMessage, workerapi.AgentMethodControlSession, workerapi.AgentMethodWaitTurn, workerapi.AgentMethodInspectSession, workerapi.AgentMethodListSessions} {
		request := workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: string(method), AuthorityGeneration: 1, TurnID: origin.TurnID.String(), Method: method, Payload: json.RawMessage(`{}`)}
		var response workerapi.AgentOperationResponse
		client.post(t, "/worker/v1/sessions/operations", request, 200, &response)
		if response.Error == nil || response.Error.Code != "unsupported_operation" {
			t.Fatalf("operation without tool envelope %s: %+v", method, response)
		}
	}
}

func TestWorkerAgentDeletedPinnedSpawnIsTerminalRejection(t *testing.T) {
	f, _, _ := runtimeAdmissionOrigin(t)
	target := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO agents(environment_id,id,key,deleted_at) VALUES($1,$2,'deleted-child',clock_timestamp()); INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init) SELECT environment_id,$2,deployment_id,template_id,false FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3`, pgx.QueryExecModeSimpleProtocol, f.Environment, target, f.Agent)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	secret := seedHostSecret(t, f.Pool, f.Worker)
	payload, _ := json.Marshal(map[string]any{"tool": "spawn", "arguments": map[string]any{"agent": "deleted-child", "input": json.RawMessage(`[{"type":"text","text":"work"}]`), "idempotencyKey": "deleted-child"}})
	response, err := secret.client(t, server.URL).AgentOperation(t.Context(), workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: "deleted-target", AuthorityGeneration: 1, Method: workerapi.AgentMethodRuntimeMCP, Payload: payload})
	if err != nil || response.Error == nil || response.Error.Code != "agent_deleted" {
		t.Fatalf("deleted spawn was not a terminal tool rejection: %+v %v", response, err)
	}
}
