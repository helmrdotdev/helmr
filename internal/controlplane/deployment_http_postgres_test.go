package controlplane

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestDeploymentHTTPPostgresSessionRoutes(t *testing.T) {
	fixture := newHTTPPostgresFixture(t)
	orgID, owner := fixture.organizationOwner(t, "deployments")
	viewerID := fixture.user(t, "Viewer")
	fixture.member(t, orgID, viewerID, db.OrgMemberRoleViewer)
	viewer := fixture.session(t, viewerID, orgID)
	projectID, environmentID := deploymentHTTPEnvironment(t, fixture, orgID)
	deploymentID := deploymentHTTPDeployment(t, fixture, orgID, projectID, environmentID)
	base := fmt.Sprintf("/api/projects/%s/environments/%s", projectID, environmentID)

	expectError := func(t *testing.T, method, path, token string, status int, code string) {
		t.Helper()
		response := fixture.request(t, method, path, token, "")
		var body api.HTTPErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != status || body.Error.Code != code {
			t.Fatalf("%s %s = %d %s, want %d %s", method, path, response.Code, response.Body.String(), status, code)
		}
	}
	expectDeployment := func(t *testing.T, method, path string) {
		t.Helper()
		response := fixture.request(t, method, path, owner, "")
		var body api.DeploymentResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || body.ID != deploymentID.String() {
			t.Fatalf("%s %s = %d %s, want deployment %s", method, path, response.Code, response.Body.String(), deploymentID)
		}
	}

	finalize := fmt.Sprintf(`{"idempotency_key":"finalize","bundle_digest":"sha256:%064x"}`, 9)
	for _, test := range []struct {
		token  string
		status int
		code   string
	}{
		{viewer, http.StatusForbidden, "forbidden"},
		{owner, http.StatusBadRequest, "bad_request"},
	} {
		response := fixture.request(t, http.MethodPost, base+"/deployment-bundles/finalize", test.token, finalize)
		var body api.HTTPErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != test.status || body.Error.Code != test.code {
			t.Fatalf("finalize = %d %s, want %d %s", response.Code, response.Body.String(), test.status, test.code)
		}
	}
	expectError(t, http.MethodGet, base+"/deployments/current", owner, http.StatusNotFound, "no_current_deployment")
	expectError(t, http.MethodGet, base+"/tasks", owner, http.StatusNotFound, "no_current_deployment")
	expectError(t, http.MethodPost, base+"/deployments/"+deploymentID.String()+"/promote", viewer, http.StatusForbidden, "forbidden")
	expectError(t, http.MethodPost, base+"/deployments/"+uuid.NewV7().String()+"/promote", owner, http.StatusNotFound, "not_found")
	expectDeployment(t, http.MethodPost, base+"/deployments/"+deploymentID.String()+"/promote")

	expectDeployment(t, http.MethodGet, base+"/deployments/current")
	expectDeployment(t, http.MethodGet, base+"/deployments/"+deploymentID.String())
	expectError(t, http.MethodGet, base+"/deployments/"+uuid.NewV7().String(), owner, http.StatusNotFound, "not_found")

	list := fixture.request(t, http.MethodGet, base+"/deployments", viewer, "")
	var deployments api.ListDeploymentsResponse
	if err := json.Unmarshal(list.Body.Bytes(), &deployments); err != nil {
		t.Fatal(err)
	}
	if list.Code != http.StatusOK || len(deployments.Deployments) != 1 || deployments.Deployments[0].ID != deploymentID.String() {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}

	tasks := fixture.request(t, http.MethodGet, base+"/tasks", viewer, "")
	var taskList api.ListTasksResponse
	if err := json.Unmarshal(tasks.Body.Bytes(), &taskList); err != nil {
		t.Fatal(err)
	}
	if tasks.Code != http.StatusOK || taskList.DeploymentID != deploymentID.String() ||
		len(taskList.Tasks) != 1 || taskList.Tasks[0].ID != "daily-report" {
		t.Fatalf("tasks = %d %s", tasks.Code, tasks.Body.String())
	}
	task := fixture.request(t, http.MethodGet, base+"/tasks/daily-report?deployment_id="+deploymentID.String(), viewer, "")
	if task.Code != http.StatusOK {
		t.Fatalf("task = %d %s", task.Code, task.Body.String())
	}
	expectError(t, http.MethodGet, base+"/tasks/missing", viewer, http.StatusNotFound, "not_found")
	expectError(t, http.MethodGet, base+"/tasks?deployment_id="+uuid.NewV7().String(), viewer, http.StatusNotFound, "deployment_not_found")
}

func TestDeploymentHTTPPostgresAPIKeyRoutes(t *testing.T) {
	fixture := newHTTPPostgresFixture(t)
	orgID, _ := fixture.organizationOwner(t, "deployment-keys")
	projectID, environmentID := deploymentHTTPEnvironment(t, fixture, orgID)
	deploymentID := deploymentHTTPDeployment(t, fixture, orgID, projectID, environmentID)
	scope := auth.Scope{OrgID: orgID, ProjectID: projectID.String(), EnvironmentID: environmentID.String()}
	managerID := fixture.user(t, "Manager")
	fixture.member(t, orgID, managerID, db.OrgMemberRoleOwner)
	manager := auth.Principal{OrgID: orgID, UserID: managerID, Kind: auth.PrincipalKindSession, Role: auth.RoleOwner}
	issue := func(permission auth.Permission) string {
		t.Helper()
		issued, err := identity.IssueAPIKey(t.Context(), fixture.queries, manager, scope, identity.APIKeyInput{
			Name: string(permission), Permissions: []auth.Permission{permission},
		})
		if err != nil {
			t.Fatal(err)
		}
		return issued.Raw
	}
	reader, deployer := issue(auth.PermissionRunsRead), issue(auth.PermissionTasksDeploy)

	promote := "/v1/deployments/" + deploymentID.String() + "/promote"
	if response := fixture.request(t, http.MethodPost, promote, reader, ""); response.Code != http.StatusForbidden {
		t.Fatalf("read key promotion = %d %s", response.Code, response.Body.String())
	}
	if response := fixture.request(t, http.MethodPost, promote, deployer, ""); response.Code != http.StatusOK {
		t.Fatalf("deploy key promotion = %d %s", response.Code, response.Body.String())
	}
	current := fixture.request(t, http.MethodGet, "/v1/deployments/current", reader, "")
	var body api.DeploymentResponse
	if err := json.Unmarshal(current.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if current.Code != http.StatusOK || body.ID != deploymentID.String() {
		t.Fatalf("current = %d %s", current.Code, current.Body.String())
	}
	if response := fixture.request(t, http.MethodGet, "/v1/deployments/current", deployer, ""); response.Code != http.StatusForbidden {
		t.Fatalf("deploy key current deployment = %d %s", response.Code, response.Body.String())
	}
}

func deploymentHTTPEnvironment(t *testing.T, fixture httpPostgresFixture, orgID uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	projectID, environmentID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), fixture.pool, `
		INSERT INTO regions (id, display_name) VALUES ('deployment-test', 'Deployment Test') ON CONFLICT DO NOTHING
	`)
	if _, err := fixture.queries.CreateProject(t.Context(), db.CreateProjectParams{
		ID: pgvalue.UUID(projectID), OrgID: pgvalue.UUID(orgID), DefaultRegionID: "deployment-test",
		Slug: "deployments", Name: "Deployments", IsDefault: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.CreateEnvironment(t.Context(), db.CreateEnvironmentParams{
		ID: pgvalue.UUID(environmentID), OrgID: pgvalue.UUID(orgID), ProjectID: pgvalue.UUID(projectID),
		Slug: "staging", Name: "Staging", ColorHex: "#315FCE", IsDefault: true,
	}); err != nil {
		t.Fatal(err)
	}
	return projectID, environmentID
}

// deploymentHTTPDeployment records a finalized Deployment declaring one Task.
func deploymentHTTPDeployment(t *testing.T, fixture httpPostgresFixture, orgID, projectID, environmentID uuid.UUID) uuid.UUID {
	t.Helper()
	deploymentID, programID := uuid.NewV7(), uuid.NewV7()
	bundleDigest := "sha256:" + fmt.Sprintf("%064x", 1)
	programDigest := "sha256:" + fmt.Sprintf("%064x", 2)
	runtimeDigest := "sha256:" + fmt.Sprintf("%064x", 3)
	dbtest.MustExec(t, t.Context(), fixture.pool, `
		WITH lifetime AS (INSERT INTO cas_blobs (digest, size_bytes) VALUES ($2, 1), ($3, 1) ON CONFLICT DO NOTHING)
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, 'application/vnd.helmr.deployment-bundle.v0+json'),
		       ($1, $3, 1, 'application/vnd.helmr.deployment-program.v0+squashfs')
	`, orgID, bundleDigest, programDigest)
	dbtest.MustExec(t, t.Context(), fixture.pool, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs')
	`, programID, orgID, projectID, environmentID, programDigest)
	dbtest.MustExec(t, t.Context(), fixture.pool, `
		INSERT INTO deployments (
		    id, org_id, project_id, environment_id, version, bundle_digest,
		    runtime_artifact_digest, program_artifact_id, program_index_digest, queue_config
		) VALUES (
		    $1, $2, $3, $4, 'test', $5, $6, $7, decode(repeat('03', 32), 'hex'),
		    '{"formatVersion":0,"queues":[{"name":"default"}]}'::jsonb
		)
	`, deploymentID, orgID, projectID, environmentID, bundleDigest, runtimeDigest, programID)
	manifest, digest, err := definition.CanonicalManifestAndDigest([]byte(
		`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), fixture.pool, `
		INSERT INTO deployment_definitions (
		    id, environment_id, deployment_id, kind, declared_id, manifest_version, manifest, manifest_digest
		) VALUES ($1, $2, $3, 'task', 'daily-report', 0, $4::jsonb, $5)
	`, uuid.NewV7(), environmentID, deploymentID, manifest, digest[:])
	return deploymentID
}
