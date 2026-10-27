package controlplane

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func runtimeDiscoveryEnvironment(t *testing.T, original sessiontest.Fixture) sessiontest.Fixture {
	t.Helper()
	f := sessiontest.Fixture{Pool: original.Pool, Environment: uuid.NewV7(), Deployment: uuid.NewV7(), Agent: uuid.NewV7(), Computer: uuid.NewV7()}
	dbtest.MustExec(t, t.Context(), f.Pool, `
      INSERT INTO environments(id,org_id,project_id,slug,name,color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at)
        SELECT $2,org_id,project_id,$2::text,'Other','#112233',max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at FROM environments WHERE id=$1;
      INSERT INTO deployments(environment_id,id,bundle_digest) SELECT $2,$3,bundle_digest FROM deployments WHERE environment_id=$1 AND id=$6;
      INSERT INTO image_specs(environment_id,id,spec_digest,spec,seed) SELECT $2,$3,spec_digest,spec,seed FROM image_specs WHERE environment_id=$1 AND id=$6;
      INSERT INTO templates(environment_id,id,key) VALUES($2,$3,'fixture-computer');
 INSERT INTO template_definitions(environment_id,deployment_id,template_id,image_spec_id,cpu_millis,memory_bytes,disk_bytes,network_policy,prepare_network_policy) VALUES($2,$3,$3,$3,1000,536870912,NULL,'{"kind":"ordinary"}','{"kind":"block"}');
      INSERT INTO agents(environment_id,id,key) VALUES($2,$4,'agent');
      INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy) VALUES($2,$4,$3,$3,false,'{"kind":"unrestricted"}');
      INSERT INTO computers(environment_id,id,image_spec_id,preparation_deadline_at,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes) VALUES($2,$5,$3,clock_timestamp()+interval '1 hour',$3,$3,1000,536870912,NULL,34359738368);
    `, pgx.QueryExecModeSimpleProtocol, original.Environment, f.Environment, f.Deployment, f.Agent, f.Computer, original.Deployment)
	return f
}

func TestWorkerAgentMCPListsEnvironmentComputers(t *testing.T) {
	f, origin, execution := runtimeAdmissionOrigin(t)
	foreign := runtimeDiscoveryEnvironment(t, f)
	// More than one page, including Computers with no Session relationship.
	for range 51 {
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes) SELECT environment_id,$2,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes FROM computers WHERE environment_id=$1 AND id=$3`, f.Environment, uuid.NewV7(), f.Computer)
	}
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	request := workerapi.AgentOperationRequest{MCPScopeID: f.MCPScope.String(), Session: runtimeTestSession(f), RequestID: "list", AuthorityGeneration: 1, Method: workerapi.AgentMethodRuntimeMCP}
	call := func(cursor string) workerapi.AgentOperationResponse {
		t.Helper()
		request.Payload, _ = json.Marshal(map[string]any{"tool": "list_computers", "arguments": map[string]string{"cursor": cursor}})
		response, err := client.AgentOperation(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		response := call(cursor)
		if response.Error != nil {
			t.Fatalf("list: %+v", response.Error)
		}
		var page api.ListComputersResponse
		if err := json.Unmarshal(response.Value, &page); err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Computers {
			if seen[item.ID] || item.ID == foreign.Computer.String() {
				t.Fatalf("duplicate or foreign Computer %s", item.ID)
			}
			seen[item.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 52 || !seen[f.Computer.String()] {
		t.Fatalf("Computer set: %d", len(seen))
	}
	if response := call("invalid"); response.Error == nil || response.Error.Code != "invalid_arguments" {
		t.Fatalf("invalid cursor: %+v", response)
	}
	// Ordinary Turn closure does not revoke the Session-bound connection.
	if err := closeRuntimeProcessing(t, f.Pool, execution, origin.TurnID); err != nil {
		t.Fatal(err)
	}
	if response := call(cursor); response.Error != nil {
		t.Fatalf("after Turn: %+v", response)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET authority_generation=2 WHERE id=$1`, f.Session)
	if response := call(cursor); response.Error == nil || response.Error.Code != "authority_changed" {
		t.Fatalf("stale page: %+v", response)
	}
	request.AuthorityGeneration = 2
	if response := call(cursor); response.Error != nil {
		t.Fatalf("fresh page: %+v", response)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','test')`, f.Environment, uuid.NewV7(), f.Session)
	if response := call(cursor); response.Error == nil || response.Error.Code != "authority_changed" {
		t.Fatalf("held page: %+v", response)
	}
}

func TestWorkerAgentMCPListsPinnedAgentDefinitions(t *testing.T) {
	f, origin, execution := runtimeAdmissionOrigin(t)
	foreign := runtimeDiscoveryEnvironment(t, f)
	addDefinition := func(f sessiontest.Fixture, key string) {
		t.Helper()
		id := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,$3)`, f.Environment, id, key)
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy)
            SELECT environment_id,$2,deployment_id,template_id,has_init,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND agent_id=$3`, f.Environment, id, f.Agent)
	}
	for i := range 51 {
		addDefinition(f, fmt.Sprintf("worker-%02d", i))
	}
	addDefinition(foreign, "foreign-only")
	// Promotion cannot replace the Session's pinned definition set.
	promoted := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `
      INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$3,'sha256:'||repeat('2',64));
      INSERT INTO template_definitions(environment_id,deployment_id,template_id,image_spec_id,cpu_millis,memory_bytes,disk_bytes,network_policy,prepare_network_policy)
        SELECT environment_id,$3,template_id,image_spec_id,cpu_millis,memory_bytes,disk_bytes,network_policy,prepare_network_policy FROM template_definitions WHERE environment_id=$1 AND deployment_id=$2;
      INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy)
        SELECT environment_id,agent_id,$3,template_id,has_init,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND deployment_id=$2 AND agent_id=$4;
      UPDATE environments SET current_deployment_id=$3 WHERE id=$1;
    `, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Deployment, promoted, f.Agent)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	request := workerapi.AgentOperationRequest{MCPScopeID: f.MCPScope.String(), Session: runtimeTestSession(f), RequestID: "list-agents", AuthorityGeneration: 1, Method: workerapi.AgentMethodRuntimeMCP}
	operation := "spawn"
	call := func(cursor string) workerapi.AgentOperationResponse {
		t.Helper()
		request.Payload, _ = json.Marshal(map[string]any{"tool": "list_agents", "arguments": map[string]string{"operation": operation, "cursor": cursor}})
		response, err := client.AgentOperation(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		response := call(cursor)
		if response.Error != nil {
			t.Fatalf("list: %+v", response.Error)
		}
		var page agent.RuntimeAgentPage
		if err := json.Unmarshal(response.Value, &page); err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Agents {
			if seen[item.ID] || item.ID == "foreign-only" || item.ID == "promoted-only" {
				t.Fatalf("duplicate or foreign Agent %s", item.ID)
			}
			seen[item.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 52 || !seen["agent"] {
		t.Fatalf("Agent set: %d", len(seen))
	}
	getter := workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: "narrow-discovery", AuthorityGeneration: 1, Method: workerapi.AgentMethodGetMCPConnection, Payload: json.RawMessage(`{"policy":{"allow":[{"operation":"spawn","agents":["worker-50"]}]}}`)}
	scoped, err := client.AgentOperation(t.Context(), getter)
	if err != nil || scoped.Error != nil {
		t.Fatalf("scope %+v %v", scoped, err)
	}
	var scope struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(scoped.Value, &scope); err != nil {
		t.Fatal(err)
	}
	request.MCPScopeID = scope.ID
	only := call("")
	var narrow agent.RuntimeAgentPage
	if only.Error != nil || json.Unmarshal(only.Value, &narrow) != nil || len(narrow.Agents) != 1 || narrow.Agents[0].ID != "worker-50" || narrow.NextCursor != "" {
		t.Fatalf("scope must filter before pagination: %+v %s", only, only.Value)
	}
	request.MCPScopeID = f.MCPScope.String()
	operation = "start"
	if response := call(cursor); response.Error == nil || response.Error.Code != "invalid_arguments" {
		t.Fatalf("mixed operation cursor: %+v", response)
	}
	response := call("")
	var current agent.RuntimeAgentPage
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	if err := json.Unmarshal(response.Value, &current); err != nil || current.DeploymentID != promoted || len(current.Agents) != 1 || current.Agents[0].ID != "agent" {
		t.Fatalf("current start definitions: %+v %v", current, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.Environment, f.Deployment)
	firstPage := call("")
	var beforePromotion agent.RuntimeAgentPage
	if firstPage.Error != nil || json.Unmarshal(firstPage.Value, &beforePromotion) != nil || beforePromotion.NextCursor == "" {
		t.Fatalf("start pagination: %+v", firstPage)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.Environment, promoted)
	if response := call(beforePromotion.NextCursor); response.Error == nil || response.Error.Code != "invalid_arguments" {
		t.Fatalf("promotion mixed snapshots: %+v", response)
	}
	operation = "spawn"
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.Environment)
	if response := call(cursor); response.Error != nil {
		t.Fatalf("no current Deployment: %+v", response)
	}
	if response := call("invalid cursor"); response.Error == nil || response.Error.Code != "invalid_arguments" {
		t.Fatalf("invalid cursor: %+v", response)
	}
	if err := closeRuntimeProcessing(t, f.Pool, execution, origin.TurnID); err != nil {
		t.Fatal(err)
	}
	if response := call(cursor); response.Error != nil {
		t.Fatalf("after Turn: %+v", response)
	}
}
