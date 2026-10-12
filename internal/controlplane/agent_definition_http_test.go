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
 INSERT INTO agents(environment_id,id,key) VALUES($2,$3,'zebra');
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,setup,triggers) VALUES($2,$3,$4,$4,false,'{}');
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
	base := fmt.Sprintf("/api/projects/%s/environments/%s/agent-definitions", project, f.Environment)
	get := func(path, token string, status int) []byte {
		t.Helper()
		r := h.request(t, http.MethodGet, path, token, "")
		if r.Code != status {
			t.Fatalf("%s: %d %s, want %d", path, r.Code, r.Body.String(), status)
		}
		return r.Body.Bytes()
	}

	computerBase := strings.TrimSuffix(base, "/agent-definitions") + "/template-definitions"
	var definitions api.ListTemplateDefinitionsResponse
	if err := json.Unmarshal(get(computerBase, owner, http.StatusOK), &definitions); err != nil {
		t.Fatal(err)
	}
	if definitions.DeploymentID != f.Deployment.String() || len(definitions.TemplateDefinitions) != 1 || definitions.TemplateDefinitions[0].ID != "fixture-computer" {
		t.Fatalf("template definitions: %+v", definitions)
	}
	var templateDefinition api.TemplateDefinition
	if err := json.Unmarshal(get(computerBase+"/fixture-computer", owner, http.StatusOK), &templateDefinition); err != nil {
		t.Fatal(err)
	}
	if templateDefinition.ID != "fixture-computer" || templateDefinition.DeploymentID != f.Deployment.String() {
		t.Fatalf("template definition: %+v", templateDefinition)
	}
	get(strings.TrimSuffix(base, "/agent-definitions")+"/sandboxes", owner, http.StatusNotFound)
	get(strings.TrimSuffix(base, "/agent-definitions")+"/agents", owner, http.StatusNotFound)
	get(strings.TrimSuffix(base, "/agent-definitions")+"/agents/agent", owner, http.StatusNotFound)
	get(base, "", http.StatusUnauthorized)
	viewer := h.user(t, "Viewer")
	h.member(t, org, viewer, db.OrgMemberRoleViewer)
	get(base, h.session(t, viewer, org), http.StatusOK)
	var first api.ListAgentDefinitionsResponse
	if err := json.Unmarshal(get(base+"?limit=1", owner, http.StatusOK), &first); err != nil {
		t.Fatal(err)
	}
	if first.DeploymentID != f.Deployment.String() || len(first.AgentDefinitions) != 1 || first.AgentDefinitions[0].ID != "agent" || first.AgentDefinitions[0].AgentID != f.Agent.String() || first.AgentDefinitions[0].TemplateID != "fixture-computer" || first.NextCursor == "" {
		t.Fatalf("first page: %+v", first)
	}
	// Removing the mutable current selection must not move the cursor's snapshot.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.Environment)
	var second api.ListAgentDefinitionsResponse
	if err := json.Unmarshal(get(base+"?cursor="+url.QueryEscape(first.NextCursor), owner, http.StatusOK), &second); err != nil {
		t.Fatal(err)
	}
	if second.DeploymentID != first.DeploymentID || len(second.AgentDefinitions) != 1 || second.AgentDefinitions[0].ID != "zebra" || second.NextCursor != "" {
		t.Fatalf("second page: %+v", second)
	}
	get(computerBase+"?cursor="+url.QueryEscape(first.NextCursor), owner, http.StatusBadRequest)
	var pinned api.AgentSession
	sessionPath := strings.TrimSuffix(base, "/agent-definitions") + "/sessions/" + f.Session.String()
	if err := json.Unmarshal(get(sessionPath, owner, http.StatusOK), &pinned); err != nil {
		t.Fatal(err)
	}
	if pinned.AgentDefinitionID != "agent" || pinned.TemplateDefinitionID != "fixture-computer" || pinned.DeploymentID != f.Deployment.String() {
		t.Fatalf("HTTP pinned definitions: %+v", pinned)
	}

	selected := "?deployment_id=" + f.Deployment.String()
	var item api.AgentDefinition
	if err := json.Unmarshal(get(base+"/agent"+selected, owner, http.StatusOK), &item); err != nil {
		t.Fatal(err)
	}
	if item.ID != "agent" || item.AgentID != f.Agent.String() || item.DeploymentID != first.DeploymentID || item.TemplateID != "fixture-computer" {
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
	get("/v1/agent-definitions"+selected, reader, http.StatusOK)
	get("/v1/agent-definitions/agent"+selected, reader, http.StatusOK)
	get("/v1/agent-definitions"+selected, issue(auth.PermissionSessionsCreate), http.StatusForbidden)
	dbtest.MustExec(t, t.Context(), f.Pool, `
 UPDATE environments SET current_deployment_id=$2 WHERE id=$1;
 UPDATE computer_preparation_specs SET seed=jsonb_build_object('profile',$3::text) WHERE environment_id=$1;
 UPDATE template_definitions SET resources='{"milliCpu":1000,"memoryMiB":512}' WHERE environment_id=$1;
 `, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Deployment, definition.ComputerSeedProfile)
	var resourceList api.ListTemplateDefinitionsResponse
	if err := json.Unmarshal(get("/v1/template-definitions", reader, http.StatusOK), &resourceList); err != nil {
		t.Fatal(err)
	}
	if len(resourceList.TemplateDefinitions) != 1 {
		t.Fatalf("list resources: %+v", resourceList)
	}
	var listedResources definition.ResourcesManifest
	if err := json.Unmarshal(resourceList.TemplateDefinitions[0].Resources, &listedResources); err != nil || listedResources.MemoryMiB != 512 || listedResources.MilliCPU != 1000 {
		t.Fatalf("list resources: %+v %v", listedResources, err)
	}

	var resourceDefinition api.TemplateDefinition
	if err := json.Unmarshal(get("/v1/template-definitions/fixture-computer", reader, http.StatusOK), &resourceDefinition); err != nil {
		t.Fatal(err)
	}
	var resources struct {
		CPU    int `json:"milliCpu"`
		Memory int `json:"memoryMiB"`
	}
	if err := json.Unmarshal(resourceDefinition.Resources, &resources); err != nil || resources.CPU != 1000 || resources.Memory != 512 {
		t.Fatalf("definition resources: %+v %v", resources, err)
	}
	get("/v1/sandboxes", reader, http.StatusNotFound)
	get("/v1/template-definitions", issue(auth.PermissionSessionsCreate), http.StatusForbidden)
	denied := h.request(t, http.MethodPost, "/v1/computers", reader, `{"template":"fixture-computer"}`)
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
	input, err := json.Marshal(map[string]string{"url": sdkServer.URL, "apiKey": sdkKey.Raw, "deploymentId": f.Deployment.String(), "agentId": f.Agent.String()})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "bun", script)
	command.Stdin = bytes.NewReader(input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Computer SDK HTTP: %v\n%s", err, output)
	}

}
