package controlplane

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"net/http/httptest"
	"testing"
)

func TestWorkerAgentMCPGetterEnforcesCeilingAndExplicitArguments(t *testing.T) {
	f := sessiontest.New(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM session_mcp_scopes WHERE environment_id=$1`, f.Environment)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE agent_definitions SET runtime_mcp_policy='{"kind":"allow","rules":[{"operation":"list_computers"}]}' WHERE environment_id=$1`, f.Environment)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	request := workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: "descriptor", AuthorityGeneration: 1, Method: workerapi.AgentMethodGetMCPConnection}
	for _, test := range []struct{ payload, code string }{
		{`null`, "invalid_runtime_mcp_policy"}, {`{"policy":null}`, "invalid_runtime_mcp_policy"}, {`{"policy":{}}`, "invalid_runtime_mcp_policy"}, {`{"policy":{"blockAll":false}}`, "invalid_runtime_mcp_policy"}, {`{"policy":{"allow":[{"operation":"spawn","agents":["missing"]}]}}`, "invalid_runtime_mcp_policy"}, {`{"policy":{"allow":[{"operation":"spawn","agents":["agent"]}]}}`, "runtime_mcp_denied"}, {`{}`, ""}, {`{"policy":{"allow":[]}}`, ""},
	} {
		request.Payload = json.RawMessage(test.payload)
		response, err := client.AgentOperation(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if test.code != "" {
			if response.Error == nil || response.Error.Code != test.code {
				t.Fatalf("payload %s: %+v", test.payload, response)
			}
			continue
		}
		if response.Error != nil {
			t.Fatal(response.Error)
		}
		var scope struct {
			ID     string          `json:"id"`
			Policy json.RawMessage `json:"policy"`
		}
		if err := json.Unmarshal(response.Value, &scope); err != nil || scope.ID == "" {
			t.Fatalf("scope %+v %v", scope, err)
		}
		invocation := request
		invocation.Method = workerapi.AgentMethodRuntimeMCP
		invocation.MCPScopeID = scope.ID
		invocation.Payload = json.RawMessage(`{"tool":"list_computers","arguments":{}}`)
		result, err := client.AgentOperation(t.Context(), invocation)
		if err != nil {
			t.Fatal(err)
		}
		if test.payload == `{}` {
			if result.Error != nil {
				t.Fatal(result.Error)
			}
		} else if result.Error == nil || result.Error.Code != "runtime_mcp_denied" {
			t.Fatalf("empty descriptor business call: %+v", result)
		}
		invocation.MCPScopeID = ""
		result, err = client.AgentOperation(t.Context(), invocation)
		if err != nil || result.Error == nil || result.Error.Code != "runtime_mcp_denied" {
			t.Fatalf("missing trusted scope: %+v %v", result, err)
		}
	}
}
