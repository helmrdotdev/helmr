package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func runtimeAdmissionOrigin(t *testing.T) (agenttest.Fixture, agent.Admission, agent.Execution) {
	t.Helper()
	f := agenttest.New(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparation_specs SET seed=jsonb_build_object('profile',$2::text) WHERE environment_id=$1;
 UPDATE computer_definitions SET resources='{"milliCpu":1000,"memoryMiB":512}' WHERE environment_id=$1;
 UPDATE computers SET preparation_spec_id=$3,origin_deployment_id=$3,origin_definition_key='fixture-computer',resources='{"milliCpu":1000,"memoryMiB":512}',storage_reservation_bytes=$4 WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, f.Environment, definition.ComputerSeedProfile, f.Deployment, disk.SeedCapacity)
	origin, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "origin", Input: json.RawMessage(`[{"type":"text","text":"1"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	execution := agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	if _, err = agent.Dispatch(t.Context(), f.Pool, execution); err != nil {
		t.Fatal(err)
	}
	return f, origin, execution
}

func TestWorkerAgentStartSpawnReconcileLostResponse(t *testing.T) {
	for _, method := range []agentv1.Operation_Method{agentv1.Operation_METHOD_START, agentv1.Operation_METHOD_SPAWN} {
		for _, placed := range []bool{false, true} {
			name := method.String() + "/fresh"
			if placed {
				name = method.String() + "/placed"
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
				payload := map[string]any{"agentId": "agent", "input": json.RawMessage(`[{"type":"text","text":"work"}]`), "idempotencyKey": "retained-decision"}
				if placed {
					payload["computerId"] = f.Computer.String()
				}
				tool := strings.ToLower(strings.TrimPrefix(method.String(), "METHOD_"))
				body, _ := json.Marshal(map[string]any{"tool": tool, "arguments": payload})
				request := workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: "retained-request", AuthorityGeneration: 1, Method: int32(agentv1.Operation_METHOD_RUNTIME_MCP), Payload: body}
				// Spawn remains pinned even when no current Deployment is selected.
				if method == agentv1.Operation_METHOD_SPAWN {
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
				if err = json.Unmarshal(first.Value, &receipt); err != nil || !receipt.Created || receipt.Sequence != 1 {
					t.Fatalf("receipt=%s err=%v", first.Value, err)
				}
				var valid bool
				err = f.Pool.QueryRow(t.Context(), `SELECT s.deployment_id=$3 AND s.requester_session_id=$4 AND s.origin_turn_id IS NULL
 AND s.causal_depth=1
 AND CASE WHEN $5 THEN s.parent_session_id=$4 AND s.root_session_id=$4 ELSE s.parent_session_id IS NULL AND s.root_session_id=s.id END
 AND CASE WHEN $6 THEN s.computer_id=$7 ELSE s.computer_id<>$7 AND c.preparation_id IS NOT NULL END
 AND (SELECT count(*) FROM turns WHERE environment_id=$1)=2
 FROM sessions s JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id) WHERE s.environment_id=$1 AND s.id=$2`, f.Environment, receipt.SessionID, f.Deployment, f.Session, method == agentv1.Operation_METHOD_SPAWN, placed, f.Computer).Scan(&valid)
				if err != nil || !valid {
					t.Fatalf("admission ownership/placement/count: %v %v", valid, err)
				}
				if err = agent.CloseProcessing(t.Context(), f.Pool, execution, origin.TurnID); err != nil {
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
	for _, method := range []agentv1.Operation_Method{agentv1.Operation_METHOD_START, agentv1.Operation_METHOD_SPAWN} {
		for _, test := range []struct{ name, payload, turn, code string }{
			{"missing key", `{"agentId":"agent","input":[{"type":"text","text":"work"}]}`, "", "invalid_arguments"},
			{"foreign origin", `{"agentId":"agent","input":[{"type":"text","text":"work"}]}`, uuid.NewV7().String(), "invalid_arguments"},
			{"bad placement", `{"agentId":"agent","input":[{"type":"text","text":"work"}],"computerId":""}`, "", "invalid_arguments"},
			{"null placement", `{"agentId":"agent","input":[{"type":"text","text":"work"}],"computerId":null}`, "", "invalid_arguments"},
			{"numeric placement", `{"agentId":"agent","input":[{"type":"text","text":"work"}],"computerId":1}`, "", "invalid_arguments"},
			{"missing input", `{"agentId":"agent"}`, "", "invalid_arguments"},
			{"deployment override", `{"agentId":"agent","input":[{"type":"text","text":"work"}],"deploymentId":"other"}`, "", "invalid_arguments"},
			{"environment override", `{"agentId":"agent","input":[{"type":"text","text":"work"}],"environmentId":"other"}`, "", "invalid_arguments"},
			{"conversation key", `{"agentId":"agent","input":[{"type":"text","text":"work"}],"sessionKey":"shared"}`, "", "invalid_arguments"},
		} {
			t.Run(method.String()+"/"+test.name, func(t *testing.T) {
				var arguments map[string]any
				json.Unmarshal([]byte(test.payload), &arguments)
				if test.name != "missing key" {
					arguments["idempotencyKey"] = "decision"
				}
				body, _ := json.Marshal(map[string]any{"tool": strings.ToLower(strings.TrimPrefix(method.String(), "METHOD_")), "arguments": arguments})
				response, err := client.AgentOperation(t.Context(), workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: uuid.NewV7().String(), AuthorityGeneration: 1, TurnID: test.turn, Method: int32(agentv1.Operation_METHOD_RUNTIME_MCP), Payload: body})
				if err != nil || response.Error == nil || response.Error.Code != test.code {
					t.Fatalf("rejection=%+v err=%v", response, err)
				}
			})
		}
	}
	var sessions, computers, preparations, turns int
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM sessions),(SELECT count(*) FROM computers),(SELECT count(*) FROM computer_preparations),(SELECT count(*) FROM turns)`).Scan(&sessions, &computers, &preparations, &turns); err != nil || sessions != 1 || computers != 1 || preparations != 0 || turns != 1 {
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
			arguments := map[string]any{"agentId": "agent", "input": json.RawMessage(`[{"type":"text","text":"work"}]`), "idempotencyKey": "durable-model-decision"}
			payload := func() json.RawMessage {
				b, _ := json.Marshal(map[string]any{"tool": tool, "arguments": arguments})
				return b
			}
			request := workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: "first-rpc", AuthorityGeneration: 1, Method: int32(agentv1.Operation_METHOD_RUNTIME_MCP), Payload: payload()}
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
			if err := agent.CloseProcessing(t.Context(), f.Pool, execution, origin.TurnID); err != nil {
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
	for _, method := range []agentv1.Operation_Method{agentv1.Operation_METHOD_START, agentv1.Operation_METHOD_SPAWN, agentv1.Operation_METHOD_ENQUEUE, agentv1.Operation_METHOD_SEND_MESSAGE, agentv1.Operation_METHOD_CONTROL_SESSION, agentv1.Operation_METHOD_WAIT_TURN, agentv1.Operation_METHOD_INSPECT_SESSION, agentv1.Operation_METHOD_LIST_SESSIONS} {
		request := workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: method.String(), AuthorityGeneration: 1, TurnID: origin.TurnID.String(), Method: int32(method), Payload: json.RawMessage(`{}`)}
		var response workerapi.AgentOperationResponse
		client.post(t, "/worker/v1/sessions/operations", request, 200, &response)
		if response.Error == nil || response.Error.Code != "unsupported_operation" {
			t.Fatalf("operation without tool envelope %s: %+v", method, response)
		}
	}
}
