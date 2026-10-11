package controlplane

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
)

func TestAgentDefinitionHTTP(t *testing.T) {
	f := agenttest.New(t)
	var org, project uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `
 UPDATE org_members SET role='owner' WHERE user_id=$1;
 INSERT INTO agents(environment_id,id,name) VALUES($2,$3,'zebra');
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,definition_key,computer_definition_key,setup,triggers) VALUES($2,$3,$4,'zebra','fixture-computer',false,'{}');
 `, pgx.QueryExecModeSimpleProtocol, f.User, f.Environment, uuid.NewV7(), f.Deployment)
	queries := db.New(f.Pool)
	cfg := completeServerConfig(t)
	cfg.PublicURL = &url.URL{Scheme: "https", Host: "console.example.test"}
	cfg.DB, cfg.TX, cfg.Auth = queries, f.Pool, identity.NewAPIKeyAuthenticator(queries)
	store, err := secret.New(queries, f.Pool, bytes.Repeat([]byte{17}, 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Secrets, cfg.SecretDelivery, cfg.SecretProxy = store, store, store
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewKeys(cfg.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	h := httpPostgresFixture{pool: f.Pool, queries: queries, handler: handler, keys: keys}
	owner := h.session(t, f.User, org)
	base := fmt.Sprintf("/api/projects/%s/environments/%s/agents", project, f.Environment)
	get := func(path, token string, status int) []byte {
		t.Helper()
		r := h.request(t, http.MethodGet, path, token, "")
		if r.Code != status {
			t.Fatalf("%s: %d %s, want %d", path, r.Code, r.Body.String(), status)
		}
		return r.Body.Bytes()
	}

	computerBase := strings.TrimSuffix(base, "/agents") + "/computer-definitions"
	var definitions api.ListComputerDefinitionsResponse
	if err := json.Unmarshal(get(computerBase, owner, http.StatusOK), &definitions); err != nil {
		t.Fatal(err)
	}
	if definitions.DeploymentID != f.Deployment.String() || len(definitions.ComputerDefinitions) != 1 || definitions.ComputerDefinitions[0].ID != "fixture-computer" {
		t.Fatalf("computer definitions: %+v", definitions)
	}
	var computerDefinition api.ComputerDefinition
	if err := json.Unmarshal(get(computerBase+"/fixture-computer", owner, http.StatusOK), &computerDefinition); err != nil {
		t.Fatal(err)
	}
	if computerDefinition.ID != "fixture-computer" || computerDefinition.DeploymentID != f.Deployment.String() {
		t.Fatalf("computer definition: %+v", computerDefinition)
	}
	get(strings.TrimSuffix(base, "/agents")+"/sandboxes", owner, http.StatusNotFound)
	get(base, "", http.StatusUnauthorized)
	viewer := h.user(t, "Viewer")
	h.member(t, org, viewer, db.OrgMemberRoleViewer)
	get(base, h.session(t, viewer, org), http.StatusOK)
	var first api.ListAgentsResponse
	if err := json.Unmarshal(get(base+"?limit=1", owner, http.StatusOK), &first); err != nil {
		t.Fatal(err)
	}
	if first.DeploymentID != f.Deployment.String() || len(first.Agents) != 1 || first.Agents[0].ID != "agent" || first.NextCursor == "" {
		t.Fatalf("first page: %+v", first)
	}
	// Removing the mutable current selection must not move the cursor's snapshot.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.Environment)
	var second api.ListAgentsResponse
	if err := json.Unmarshal(get(base+"?cursor="+url.QueryEscape(first.NextCursor), owner, http.StatusOK), &second); err != nil {
		t.Fatal(err)
	}
	if second.DeploymentID != first.DeploymentID || len(second.Agents) != 1 || second.Agents[0].ID != "zebra" || second.NextCursor != "" {
		t.Fatalf("second page: %+v", second)
	}
	get(computerBase+"?cursor="+url.QueryEscape(first.NextCursor), owner, http.StatusBadRequest)
	selected := "?deployment_id=" + f.Deployment.String()
	var item api.AgentDefinition
	if err := json.Unmarshal(get(base+"/agent"+selected, owner, http.StatusOK), &item); err != nil {
		t.Fatal(err)
	}
	if item.ID != "agent" || item.DeploymentID != first.DeploymentID {
		t.Fatalf("item: %+v", item)
	}
	get(base, owner, http.StatusNotFound)
	get(base+"/agent", owner, http.StatusNotFound)
	get(base+"/absent"+selected, owner, http.StatusNotFound)
	get(base+"?limit=101", owner, http.StatusBadRequest)
	get(base+"/agent?limit=1", owner, http.StatusBadRequest)
	get(base+"?cursor="+url.QueryEscape(first.NextCursor)+"&deployment_id="+uuid.NewV7().String(), owner, http.StatusBadRequest)
	_, other := h.organizationOwner(t, "other-org")
	get(base+selected, other, http.StatusBadRequest)
	issuer := auth.Principal{OrgID: org, UserID: f.User, Kind: auth.PrincipalKindSession, Role: auth.RoleOwner}
	scope := auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: f.Environment.String()}
	issue := func(permission auth.Permission) string {
		t.Helper()
		key, err := identity.IssueAPIKey(t.Context(), queries, issuer, scope, identity.APIKeyInput{Name: string(permission), Permissions: []auth.Permission{permission}})
		if err != nil {
			t.Fatal(err)
		}
		return key.Raw
	}
	reader := issue(auth.PermissionSessionsRead)
	get("/v1/agents"+selected, reader, http.StatusOK)
	get("/v1/agents/agent"+selected, reader, http.StatusOK)
	get("/v1/agents"+selected, issue(auth.PermissionAgentsStart), http.StatusForbidden)
	dbtest.MustExec(t, t.Context(), f.Pool, `
 UPDATE environments SET current_deployment_id=$2 WHERE id=$1;
 UPDATE computer_preparation_specs SET seed=jsonb_build_object('profile',$3::text) WHERE environment_id=$1;
 UPDATE computer_definitions SET resources='{"milliCpu":1000,"memoryMiB":512}' WHERE environment_id=$1;
 `, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Deployment, definition.ComputerSeedProfile)
	get("/v1/computer-definitions", reader, http.StatusOK)
	get("/v1/computer-definitions/fixture-computer", reader, http.StatusOK)
	get("/v1/sandboxes", reader, http.StatusNotFound)
	get("/v1/computer-definitions", issue(auth.PermissionAgentsStart), http.StatusForbidden)
	denied := h.request(t, http.MethodPost, "/v1/computer-definitions/fixture-computer/computers", reader, `{}`)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("read key created Computer: %d %s", denied.Code, denied.Body.String())
	}
	sdkKey, err := identity.IssueAPIKey(t.Context(), queries, issuer, scope, identity.APIKeyInput{Name: "computer SDK", Permissions: []auth.Permission{auth.PermissionSessionsRead, auth.PermissionComputersCreate, auth.PermissionComputersRead, auth.PermissionSecretsWrite}})
	if err != nil {
		t.Fatal(err)
	}
	sdkServer := httptest.NewServer(handler)
	defer sdkServer.Close()
	assertDefinitionCLIHTTP(t, sdkServer.URL, reader, f.Deployment.String())
	script, err := filepath.Abs("../../sdk/typescript/testdata/computer-catalog-http.ts")
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]string{"url": sdkServer.URL, "apiKey": sdkKey.Raw, "deploymentId": f.Deployment.String()})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "bun", script)
	command.Stdin = bytes.NewReader(input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Computer SDK HTTP: %v\n%s", err, output)
	}

}
