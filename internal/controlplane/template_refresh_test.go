package controlplane

import (
	"encoding/json"
	"net/http"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestTemplateRefreshHTTPPermissionsAndObservation(t *testing.T) {
	f := sessiontest.New(t)
	var org, project uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET is_owner=true WHERE user_id=$1`, f.User)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE template_definitions SET prepare=true WHERE environment_id=$1`, f.Environment)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.Environment, f.Deployment)
	queries := db.New(f.Pool)
	cfg := completeServerConfig(t)
	cfg.DB, cfg.TX, cfg.Auth = queries, f.Pool, identity.NewAPIKeyAuthenticator(f.Pool)
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewKeys(cfg.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	h := httpPostgresFixture{pool: f.Pool, queries: queries, handler: handler, keys: keys}
	issuer := f.Principal(t)
	scope := auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: f.Environment.String()}
	issue := func(permission auth.Permission) string {
		t.Helper()
		key, err := issueAPIKeyWithRole(t, t.Context(), f.Pool, issuer, scope, testAPIKeyInput{Name: string(permission) + uuid.NewV7().String(), RolePermissions: []auth.Permission{permission}})
		if err != nil {
			t.Fatal(err)
		}
		return key.Raw
	}
	writer, reader := issue(auth.PermissionTemplatesRefresh), issue(auth.PermissionTemplatesRead)
	path := "/v1/templates/fixture-computer/refresh"
	for _, tc := range []struct {
		token, body string
		status      int
	}{{reader, `{"idempotency_key":"refresh"}`, http.StatusForbidden}, {writer, `{}`, http.StatusBadRequest}, {writer, `{"idempotency_key":"refresh"}`, http.StatusAccepted}} {
		response := h.request(t, http.MethodPost, path, tc.token, tc.body)
		if response.Code != tc.status {
			t.Fatalf("refresh HTTP %d: %s", response.Code, response.Body.String())
		}
	}
	response := h.request(t, http.MethodPost, path, writer, `{"idempotency_key":"refresh"}`)
	var build api.TemplateBuildResponse
	if err := json.Unmarshal(response.Body.Bytes(), &build); err != nil || build.ImageBuildID == "" {
		t.Fatalf("receipt %s: %v", response.Body.String(), err)
	}
	readPath := "/v1/templates/fixture-computer/builds/" + build.ImageBuildID + "?deployment_id=" + build.DeploymentID
	response = h.request(t, http.MethodGet, readPath, issue(auth.PermissionAgentsUse), "")
	if response.Code != http.StatusForbidden {
		t.Fatalf("Use-only build read HTTP %d: %s", response.Code, response.Body.String())
	}
	response = h.request(t, http.MethodGet, readPath, reader, "")
	if response.Code != http.StatusOK {
		t.Fatalf("read HTTP %d: %s", response.Code, response.Body.String())
	}
	var observed api.TemplateBuildResponse
	if err := json.Unmarshal(response.Body.Bytes(), &observed); err != nil || observed.ImageBuildID != build.ImageBuildID || observed.DeploymentID != build.DeploymentID {
		t.Fatalf("observation %s: %v", response.Body.String(), err)
	}
}
