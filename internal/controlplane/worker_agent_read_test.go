package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"uuid"

	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/runtimemcp"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"strings"
)

func TestWorkerAgentTurnObservation(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	secret := seedHostSecret(t, f.Pool, f.Worker)
	client := secret.client(t, server.URL)
	origin, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "origin", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	execution := agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, WorkerHostID: f.Worker, WorkerEpoch: 1, ProcessEpoch: 1, LeaseEpoch: 1, AuthorityGeneration: 1}
	if _, err := agent.Dispatch(t.Context(), f.Pool, execution); err != nil {
		t.Fatal(err)
	}
	// The caller's admitted follow-up is observable; the human-origin Turn is not.
	admission, err := client.AgentOperation(t.Context(), runtimeEnqueue(f, "followup"))
	if err != nil || admission.Error != nil {
		t.Fatalf("admission %+v %v", admission, err)
	}
	var receipt struct {
		TurnID string `json:"turnId"`
	}
	if err := json.Unmarshal(admission.Value, &receipt); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"tool": "inspect_turn", "arguments": map[string]string{"sessionId": f.Session.String(), "turnId": receipt.TurnID}})
	request := workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: uuid.NewV7().String(), AuthorityGeneration: 1, Method: int32(agentv1.Operation_METHOD_RUNTIME_MCP), Payload: body}
	observed, err := client.AgentOperation(t.Context(), request)
	if err != nil || observed.Error != nil {
		t.Fatalf("observe %+v %v", observed, err)
	}
	var state map[string]any
	if err := json.Unmarshal(observed.Value, &state); err != nil {
		t.Fatal(err)
	}
	if state["status"] != "queued" || state["id"] != receipt.TurnID || len(state) != 3 {
		t.Fatalf("observation leaked fields %s", observed.Value)
	}

	// Exercise the MCP HTTP catalog through its Session-bound worker transport.
	handler := runtimemcp.NewHandler(func(ctx context.Context, tool string, arguments json.RawMessage) (json.RawMessage, error) {
		payload, _ := json.Marshal(map[string]any{"tool": tool, "arguments": arguments})
		req := request
		req.TurnID = ""
		req.Method = int32(agentv1.Operation_METHOD_RUNTIME_MCP)
		req.Payload = payload
		req.RequestID = uuid.NewV7().String()
		response, err := client.AgentOperation(ctx, req)
		if err != nil {
			return nil, err
		}
		if response.Error != nil {
			return nil, errors.New(response.Error.Code)
		}
		return response.Value, nil
	})
	mcpRequest := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait_turn","arguments":{"sessionId":"`+f.Session.String()+`","turnId":"`+receipt.TurnID+`","timeoutMs":10}}}`))
	mcpRequest.Header.Set("Content-Type", "application/json")
	mcpRequest.Header.Set("Accept", "application/json, text/event-stream")
	mcpRequest.Header.Set("MCP-Protocol-Version", "2025-11-25")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, mcpRequest)
	if response.Code != 200 {
		t.Fatalf("MCP %d %s", response.Code, response.Body.String())
	}
	var rpc struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct{ Text string }
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &rpc); err != nil {
		t.Fatal(err)
	}
	if rpc.Result.IsError || len(rpc.Result.Content) != 1 || rpc.Result.Content[0].Text != `{"status":"timeout"}` {
		t.Fatalf("MCP result %s", response.Body.String())
	}
	var status string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, receipt.TurnID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("timeout changed work %s %v", status, err)
	}
	request.Payload, _ = json.Marshal(map[string]any{"tool": "inspect_turn", "arguments": map[string]string{"sessionId": f.Session.String(), "turnId": origin.TurnID.String()}})
	denied, err := client.AgentOperation(t.Context(), request)
	if err != nil || denied.Error == nil {
		t.Fatalf("unrelated read %+v %v", denied, err)
	}
	request.Payload = body
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET authority_generation=2 WHERE id=$1`, f.Session)
	denied, err = client.AgentOperation(t.Context(), request)
	if err != nil || denied.Error == nil || denied.Error.Code != "authority_changed" {
		t.Fatalf("stale read %+v %v", denied, err)
	}
}
