package controlplane

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func runtimeDiscoveryEnvironment(t *testing.T, original agenttest.Fixture) agenttest.Fixture {
	t.Helper()
	f := agenttest.Fixture{Pool: original.Pool, Environment: uuid.NewV7(), Deployment: uuid.NewV7(), Agent: uuid.NewV7(), Computer: uuid.NewV7()}
	dbtest.MustExec(t, t.Context(), f.Pool, `
      INSERT INTO environments(history_retention_mode,id,org_id,project_id,slug,name,color_hex)
        SELECT history_retention_mode,$2,org_id,project_id,$2::text,'Other','#112233' FROM environments WHERE id=$1;
      INSERT INTO deployments(environment_id,id,bundle_digest) SELECT $2,$3,bundle_digest FROM deployments WHERE environment_id=$1 AND id=$6;
      INSERT INTO computer_preparation_specs(environment_id,id,spec_digest,spec,seed) SELECT $2,$3,spec_digest,spec,seed FROM computer_preparation_specs WHERE environment_id=$1 AND id=$6;
      INSERT INTO computer_definitions(environment_id,deployment_id,definition_key,preparation_spec_id,resources) VALUES($2,$3,'fixture-computer',$3,'{}');
      INSERT INTO agents(environment_id,id,name) VALUES($2,$4,'agent');
      INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,definition_key,computer_definition_key,setup,triggers) VALUES($2,$4,$3,'agent','fixture-computer',false,'{}');
      INSERT INTO computers(environment_id,id,preparation_spec_id,preparation_deadline_at) VALUES($2,$5,$3,clock_timestamp()+interval '1 hour');
    `, pgx.QueryExecModeSimpleProtocol, original.Environment, f.Environment, f.Deployment, f.Agent, f.Computer, original.Deployment)
	return f
}

func TestWorkerAgentMCPListsEnvironmentComputers(t *testing.T) {
	f, origin, execution := runtimeAdmissionOrigin(t)
	foreign := runtimeDiscoveryEnvironment(t, f)
	// More than one page, including Computers with no Session relationship.
	for range 51 {
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest)
        SELECT environment_id,$2,initial_root_id,initial_root_digest FROM computers WHERE environment_id=$1 AND id=$3`, f.Environment, uuid.NewV7(), f.Computer)
	}
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	request := workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: "list", AuthorityGeneration: 1, Method: int32(agentv1.Operation_METHOD_RUNTIME_MCP)}
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
	if err := agent.CloseProcessing(t.Context(), f.Pool, execution, origin.TurnID); err != nil {
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
	addDefinition := func(f agenttest.Fixture, key string) {
		t.Helper()
		id := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO agents(environment_id,id,name) VALUES($1,$2,$3)`, f.Environment, id, key)
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,definition_key,computer_definition_key,setup,triggers)
            SELECT environment_id,$2,deployment_id,$3,computer_definition_key,setup,triggers FROM agent_definitions WHERE environment_id=$1 AND agent_id=$4`, f.Environment, id, key, f.Agent)
	}
	for i := range 51 {
		addDefinition(f, fmt.Sprintf("worker-%02d", i))
	}
	addDefinition(foreign, "foreign-only")
	// Promotion cannot replace the Session's pinned definition set.
	promoted := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `
      INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$3,'sha256:'||repeat('2',64));
      INSERT INTO computer_definitions(environment_id,deployment_id,definition_key,preparation_spec_id,resources)
        SELECT environment_id,$3,definition_key,preparation_spec_id,resources FROM computer_definitions WHERE environment_id=$1 AND deployment_id=$2;
      INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,definition_key,computer_definition_key,setup,triggers)
        SELECT environment_id,agent_id,$3,'promoted-only',computer_definition_key,setup,triggers FROM agent_definitions WHERE environment_id=$1 AND deployment_id=$2 AND agent_id=$4;
      UPDATE environments SET current_deployment_id=$3 WHERE id=$1;
    `, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Deployment, promoted, f.Agent)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	request := workerapi.AgentOperationRequest{Session: runtimeTestSession(f), RequestID: "list-agents", AuthorityGeneration: 1, Method: int32(agentv1.Operation_METHOD_RUNTIME_MCP)}
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
	operation = "start"
	if response := call(cursor); response.Error == nil || response.Error.Code != "invalid_arguments" {
		t.Fatalf("mixed operation cursor: %+v", response)
	}
	response := call("")
	var current agent.RuntimeAgentPage
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	if err := json.Unmarshal(response.Value, &current); err != nil || current.DeploymentID != promoted || len(current.Agents) != 1 || current.Agents[0].ID != "promoted-only" {
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
	if err := agent.CloseProcessing(t.Context(), f.Pool, execution, origin.TurnID); err != nil {
		t.Fatal(err)
	}
	if response := call(cursor); response.Error != nil {
		t.Fatalf("after Turn: %+v", response)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployments SET execution_revoked_at=clock_timestamp() WHERE id=$1`, f.Deployment)
	if response := call(cursor); response.Error == nil || response.Error.Code != "authority_changed" {
		t.Fatalf("revoked page: %+v", response)
	}
}
