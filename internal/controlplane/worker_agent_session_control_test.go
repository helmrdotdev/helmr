package controlplane

import (
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerAgentSessionControls(t *testing.T) {
	f, origin, execution := runtimeAdmissionOrigin(t)
	client := newWorkerHTTPClient(t, newPostgresServer(t, f.Pool), f.Pool, f.Worker)
	request := workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: "spawn", AuthorityGeneration: 1, TurnID: origin.TurnID.String(), Method: int32(agentv1.Operation_METHOD_SPAWN)}
	call := func(r workerapi.AgentOperationRequest, body any, code string) map[string]string {
		t.Helper()
		raw, _ := json.Marshal(body)
		var arguments map[string]any
		json.Unmarshal(raw, &arguments)
		tool := "spawn"
		switch agentv1.Operation_Method(r.Method) {
		case agentv1.Operation_METHOD_START:
			tool = "start"
		case agentv1.Operation_METHOD_CONTROL_SESSION:
			tool, _ = arguments["kind"].(string)
			tool += "_session"
			delete(arguments, "kind")
		}
		if _, ok := arguments["idempotencyKey"]; !ok {
			arguments["idempotencyKey"] = r.RequestID
		}
		r.Method = int32(agentv1.Operation_METHOD_RUNTIME_MCP)
		r.TurnID = ""
		r.Payload, _ = json.Marshal(map[string]any{"tool": tool, "arguments": arguments})
		var response workerapi.AgentOperationResponse
		client.post(t, "/worker/v1/sessions/operations", r, 200, &response)
		if code != "" {
			if response.Error == nil || response.Error.Code != code {
				t.Fatalf("expected %s: %+v", code, response)
			}
			return nil
		}
		if response.Error != nil {
			t.Fatalf("operation: %+v", response.Error)
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(response.Value, &result); err != nil {
			t.Fatal(err)
		}
		values := map[string]string{}
		for k, v := range result {
			var s string
			if json.Unmarshal(v, &s) == nil {
				values[k] = s
			}
		}
		return values
	}
	admission := map[string]any{"agentId": "agent", "input": json.RawMessage(`[]`), "computerId": f.Computer.String()}
	child := call(request, admission, "")
	request.Method = int32(agentv1.Operation_METHOD_START)
	request.RequestID = "independent"
	independent := call(request, admission, "")
	request.Method = int32(agentv1.Operation_METHOD_CONTROL_SESSION)
	request.RequestID = "interrupt"
	body := map[string]any{"sessionId": child["sessionId"], "kind": "interrupt"}
	first := call(request, body, "")
	if first["status"] != "accepted" || first["sessionId"] != child["sessionId"] || first["id"] == "" || first["holdId"] == "" {
		t.Fatalf("receipt: %+v", first)
	}
	if got := call(request, body, ""); got["id"] != first["id"] || got["holdId"] != first["holdId"] {
		t.Fatalf("retry: %+v", got)
	}
	resume := request
	resume.RequestID = "resume"
	resumed := call(resume, map[string]any{"sessionId": child["sessionId"], "kind": "resume", "holdId": first["holdId"]}, "")
	if resumed["holdId"] != first["holdId"] {
		t.Fatalf("resume: %+v", resumed)
	}
	if got := call(request, body, ""); got["id"] != first["id"] || got["holdId"] != first["holdId"] {
		t.Fatalf("released retry: %+v", got)
	}
	call(resume, map[string]any{"sessionId": child["sessionId"], "kind": "resume", "holdId": uuid.New().String()}, "idempotency_conflict")
	explicit := request
	explicit.RequestID = "explicit-first"
	explicitBody := map[string]any{"sessionId": child["sessionId"], "kind": "interrupt", "idempotencyKey": "authored"}
	explicitReceipt := call(explicit, explicitBody, "")
	explicit.RequestID = "explicit-retry"
	if got := call(explicit, explicitBody, ""); got["id"] != explicitReceipt["id"] {
		t.Fatalf("authored retry: %+v", got)
	}
	for _, target := range []string{f.Session.String(), independent["sessionId"]} {
		call(request, map[string]any{"sessionId": target, "kind": "cancel"}, "authority_changed")
	}
	for _, bad := range []map[string]any{
		{"sessionId": child["sessionId"], "kind": "resume"},
		{"sessionId": child["sessionId"], "kind": "cancel", "holdId": first["holdId"]},
		{"sessionId": child["sessionId"], "kind": "close", "holdId": "bad"},
		{"sessionId": child["sessionId"], "kind": "interrupt", "callerId": f.Session.String()},
	} {
		call(request, bad, "invalid_arguments")
	}
	childID, err := uuid.Parse(child["sessionId"])
	if err != nil {
		t.Fatal(err)
	}
	human, err := agent.ControlSession(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: childID, Kind: "interrupt", RetryKey: "human"})
	if err != nil {
		t.Fatal(err)
	}
	humanResume := request
	humanResume.RequestID = "human-resume"
	call(humanResume, map[string]any{"sessionId": child["sessionId"], "kind": "resume", "holdId": human.HoldID.String()}, "authority_changed")
	for _, kind := range []string{"close", "cancel"} {
		r := request
		r.RequestID = kind
		receipt := call(r, map[string]any{"sessionId": child["sessionId"], "kind": kind}, "")
		if receipt["holdId"] != "" || receipt["status"] != "accepted" {
			t.Fatalf("%s receipt: %+v", kind, receipt)
		}
	}
	var cancelled bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT status='cancelled' AND started_at IS NULL FROM turns WHERE id=$1`, child["turnId"]).Scan(&cancelled); err != nil || !cancelled {
		t.Fatalf("queued cancellation: %v %v", cancelled, err)
	}
	if err = agent.CloseProcessing(t.Context(), f.Pool, execution, origin.TurnID); err != nil {
		t.Fatal(err)
	}
	if got := call(request, body, ""); got["id"] != first["id"] {
		t.Fatalf("closed caller receipt: %+v", got)
	}
	request.RequestID = "interrupt"
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET authority_generation=2 WHERE id=$1`, f.Session)
	call(request, body, "authority_changed")
}
