package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/runtimemcp"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerAgentSessionDiscovery(t *testing.T) {
	f, origin, execution := runtimeAdmissionOrigin(t)
	apiHandler := newPostgresServer(t, f.Pool)
	client := newWorkerHTTPClient(t, apiHandler, f.Pool, f.Worker)
	call := func(method agentv1.Operation_Method, turn string, body any) workerapi.AgentOperationResponse {
		t.Helper()
		payload, _ := json.Marshal(body)
		request := workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: uuid.NewV7().String(), AuthorityGeneration: 1, TurnID: turn, Method: int32(method), Payload: payload}
		var response workerapi.AgentOperationResponse
		client.post(t, "/worker/v1/sessions/operations", request, 200, &response)
		return response
	}
	create := func(method agentv1.Operation_Method) (session, turn string) {
		t.Helper()
		response := call(agentv1.Operation_METHOD_RUNTIME_MCP, "", map[string]any{"tool": strings.ToLower(strings.TrimPrefix(method.String(), "METHOD_")), "arguments": map[string]any{"agentId": "agent", "input": json.RawMessage(`[]`), "computerId": f.Computer.String(), "idempotencyKey": "create-" + method.String()}})
		var receipt struct{ SessionID, TurnID string }
		if response.Error != nil || json.Unmarshal(response.Value, &receipt) != nil || receipt.SessionID == "" || receipt.TurnID == "" {
			t.Fatalf("creation: %+v", response)
		}
		return receipt.SessionID, receipt.TurnID
	}
	owned, ownTurn := create(agentv1.Operation_METHOD_SPAWN)
	independent, initial := create(agentv1.Operation_METHOD_START)
	response := call(agentv1.Operation_METHOD_RUNTIME_MCP, "", map[string]any{"tool": "list_sessions", "arguments": map[string]any{"relation": "owned"}})
	var page api.AgentSessionsPage
	if response.Error != nil || json.Unmarshal(response.Value, &page) != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != owned || page.Sessions[0].InitialTurn == nil || page.Sessions[0].InitialTurn.ID != ownTurn {
		t.Fatalf("SDK discovery: %s %+v", response.Value, response.Error)
	}
	for _, body := range []map[string]any{{"relation": "requested", "callerId": f.Session.String()}, {"relation": "all"}, {"relation": "owned", "limit": 0}, {"relation": "owned", "cursor": "invalid"}} {
		response = call(agentv1.Operation_METHOD_RUNTIME_MCP, "", map[string]any{"tool": "list_sessions", "arguments": body})
		if response.Error == nil || response.Error.Code != "invalid_arguments" {
			t.Fatalf("invalid discovery: %+v", response)
		}
	}
	// Session MCP stays available after an exact Turn closes.
	if err := agent.CloseProcessing(t.Context(), f.Pool, execution, origin.TurnID); err != nil {
		t.Fatal(err)
	}
	handler := runtimemcp.NewHandler(func(_ context.Context, tool string, arguments json.RawMessage) (json.RawMessage, error) {
		response := call(agentv1.Operation_METHOD_RUNTIME_MCP, "", map[string]any{"tool": tool, "arguments": arguments})
		if response.Error != nil {
			return nil, errors.New(response.Error.Code)
		}
		return response.Value, nil
	})
	mcp := func(tool string, arguments any, wantError bool) json.RawMessage {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": arguments}})
		r := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("MCP-Protocol-Version", "2025-11-25")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var rpc struct {
			Result struct {
				IsError bool
				Content []struct{ Text string }
			}
			Error json.RawMessage
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rpc) != nil || len(rpc.Error) > 0 || rpc.Result.IsError != wantError || len(rpc.Result.Content) != 1 {
			t.Fatalf("MCP: %d %s", w.Code, w.Body.String())
		}
		return json.RawMessage(rpc.Result.Content[0].Text)
	}
	first := mcp("list_sessions", map[string]any{"relation": "requested", "limit": 1}, false)
	if json.Unmarshal(first, &page) != nil || len(page.Sessions) != 1 || page.NextCursor == "" {
		t.Fatalf("MCP first: %s", first)
	}
	firstID := page.Sessions[0].ID
	second := mcp("list_sessions", map[string]any{"relation": "requested", "limit": 1, "cursor": page.NextCursor}, false)
	page = api.AgentSessionsPage{}
	if json.Unmarshal(second, &page) != nil || len(page.Sessions) != 1 || page.Sessions[0].ID == firstID || page.NextCursor != "" {
		t.Fatalf("MCP next: %s", second)
	}
	observed := mcp("inspect_session", map[string]string{"sessionId": independent}, false)
	var view api.AgentSession
	if json.Unmarshal(observed, &view) != nil || view.ID != independent || view.ParentSessionID != nil || view.RequesterSessionID == nil || *view.RequesterSessionID != f.Session.String() || view.InitialTurn == nil || view.InitialTurn.ID != initial {
		t.Fatalf("MCP recovered receipt: %s", observed)
	}
	// The external SDK uses the same retained metadata and authorized relation
	// filters through actual HTTP, without borrowing runtime caller authority.
	var org, project uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET role='owner' WHERE user_id=$1`, f.User)
	key, err := identity.IssueAPIKey(t.Context(), db.New(f.Pool), auth.Principal{OrgID: org, UserID: f.User, Kind: auth.PrincipalKindSession, Role: auth.RoleOwner}, auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: f.Environment.String()}, identity.APIKeyInput{Name: "SDK discovery", Permissions: []auth.Permission{auth.PermissionSessionsRead}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(apiHandler)
	defer server.Close()
	script, err := filepath.Abs("../../sdk/typescript/testdata/session-discovery-http.ts")
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]string{"url": server.URL, "apiKey": key.Raw, "caller": f.Session.String(), "owned": owned, "independent": independent, "initial": initial})
	command := exec.CommandContext(t.Context(), "bun", script)
	command.Stdin = bytes.NewReader(input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("SDK discovery: %v\n%s", err, output)
	}
	mcp("inspect_session", map[string]string{"sessionId": f.Session.String()}, true)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET authority_generation=2 WHERE id=$1`, f.Session)
	mcp("list_sessions", map[string]string{"relation": "requested"}, true)
}
