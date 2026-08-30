package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type workspaceExecHTTPStore struct {
	db.Querier
	want  db.GetWorkspaceExecParams
	value db.WorkspaceProcess
	calls int
}

func (s *workspaceExecHTTPStore) GetWorkspaceExec(
	_ context.Context,
	params db.GetWorkspaceExecParams,
) (db.WorkspaceProcess, error) {
	s.calls++
	if params != s.want {
		return db.WorkspaceProcess{}, pgx.ErrNoRows
	}
	return s.value, nil
}

func TestGetWorkspaceExecHTTPProjectsEveryPublicState(t *testing.T) {
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	workspaceID := uuid.NewV7()
	processID := uuid.NewV7()
	principal := workspaceExecHTTPPrincipal(orgID, projectID, environmentID)
	want := db.GetWorkspaceExecParams{
		OrgID: pgvalue.UUID(orgID), ProjectID: pgvalue.UUID(projectID),
		EnvironmentID: pgvalue.UUID(environmentID), WorkspaceID: pgvalue.UUID(workspaceID),
		ID: pgvalue.UUID(processID),
	}

	tests := []struct {
		name       string
		process    db.WorkspaceProcess
		statusCode int
		status     api.WorkspaceExecProcessStatus
	}{
		{name: "pending", process: db.WorkspaceProcess{ID: pgvalue.UUID(processID), State: db.WorkspaceProcessStatePending}, statusCode: http.StatusAccepted, status: api.WorkspaceExecProcessStatusPending},
		{name: "starting", process: db.WorkspaceProcess{ID: pgvalue.UUID(processID), State: db.WorkspaceProcessStateStarting}, statusCode: http.StatusAccepted, status: api.WorkspaceExecProcessStatusPending},
		{name: "running", process: db.WorkspaceProcess{ID: pgvalue.UUID(processID), State: db.WorkspaceProcessStateRunning}, statusCode: http.StatusAccepted, status: api.WorkspaceExecProcessStatusRunning},
		{name: "exit requested", process: db.WorkspaceProcess{ID: pgvalue.UUID(processID), State: db.WorkspaceProcessStateExitRequested}, statusCode: http.StatusAccepted, status: api.WorkspaceExecProcessStatusRunning},
		{name: "exited", process: db.WorkspaceProcess{
			ID: pgvalue.UUID(processID), State: db.WorkspaceProcessStateExited,
			ExitCode: pgtype.Int4{Int32: 17, Valid: true}, Stdout: []byte("out"), Stderr: []byte("err"),
		}, statusCode: http.StatusOK, status: api.WorkspaceExecProcessStatusExited},
		{name: "failed", process: db.WorkspaceProcess{
			ID: pgvalue.UUID(processID), State: db.WorkspaceProcessStateFailed,
			TerminalReasonCode: pgvalue.Text("workspace_exec_placement_timed_out"),
		}, statusCode: http.StatusOK, status: api.WorkspaceExecProcessStatusFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &workspaceExecHTTPStore{want: want, value: test.process}
			recorder := httptest.NewRecorder()
			(&Server{db: store}).getWorkspaceExecHTTP(
				recorder,
				workspaceExecHTTPGetRequest(workspaceID.String(), processID.String(), principal),
			)
			if recorder.Code != test.statusCode {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var response api.WorkspaceExecProcess
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.ProcessID != processID.String() || response.Status != test.status {
				t.Fatalf("response = %+v", response)
			}
			if test.status == api.WorkspaceExecProcessStatusExited &&
				(response.ExitCode == nil || *response.ExitCode != 17 || response.StdoutBase64 == nil || *response.StdoutBase64 != "b3V0" || response.StderrBase64 == nil || *response.StderrBase64 != "ZXJy") {
				t.Fatalf("exited response = %+v", response)
			}
			if test.status == api.WorkspaceExecProcessStatusFailed &&
				(response.Error == nil || response.Error.TerminalReasonCode != "workspace_exec_placement_timed_out") {
				t.Fatalf("failed response = %+v", response)
			}
		})
	}
}

func TestGetWorkspaceExecHTTPRequiresExecPermissionAndValidIDs(t *testing.T) {
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	workspaceID := uuid.NewV7()
	processID := uuid.NewV7()
	store := &workspaceExecHTTPStore{}
	server := &Server{db: store}

	viewer := auth.Actor{
		OrgID: orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleViewer,
		ProjectID: projectID.String(), EnvironmentID: environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionWorkspacesRead},
	}
	recorder := httptest.NewRecorder()
	server.getWorkspaceExecHTTP(recorder, workspaceExecHTTPGetRequest(workspaceID.String(), processID.String(), viewer))
	if recorder.Code != http.StatusForbidden || store.calls != 0 {
		t.Fatalf("viewer status=%d calls=%d body=%s", recorder.Code, store.calls, recorder.Body.String())
	}

	principal := workspaceExecHTTPPrincipal(orgID, projectID, environmentID)
	for _, test := range []struct {
		name        string
		workspaceID string
		processID   string
	}{
		{name: "workspace", workspaceID: "not-a-workspace", processID: processID.String()},
		{name: "process", workspaceID: workspaceID.String(), processID: "not-a-process"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.getWorkspaceExecHTTP(recorder, workspaceExecHTTPGetRequest(test.workspaceID, test.processID, principal))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if store.calls != 0 {
		t.Fatalf("invalid requests reached store %d times", store.calls)
	}
}

func TestGetWorkspaceExecHTTPIsolatesEveryAuthorityCoordinate(t *testing.T) {
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	workspaceID := uuid.NewV7()
	processID := uuid.NewV7()
	store := &workspaceExecHTTPStore{
		want: db.GetWorkspaceExecParams{
			OrgID: pgvalue.UUID(orgID), ProjectID: pgvalue.UUID(projectID),
			EnvironmentID: pgvalue.UUID(environmentID), WorkspaceID: pgvalue.UUID(workspaceID),
			ID: pgvalue.UUID(processID),
		},
		value: db.WorkspaceProcess{ID: pgvalue.UUID(processID), State: db.WorkspaceProcessStatePending},
	}
	server := &Server{db: store}

	tests := []struct {
		name        string
		principal   auth.Actor
		workspaceID uuid.UUID
		processID   uuid.UUID
	}{
		{name: "organization", principal: workspaceExecHTTPPrincipal(uuid.NewV7(), projectID, environmentID), workspaceID: workspaceID, processID: processID},
		{name: "project", principal: workspaceExecHTTPPrincipal(orgID, uuid.NewV7(), environmentID), workspaceID: workspaceID, processID: processID},
		{name: "environment", principal: workspaceExecHTTPPrincipal(orgID, projectID, uuid.NewV7()), workspaceID: workspaceID, processID: processID},
		{name: "workspace", principal: workspaceExecHTTPPrincipal(orgID, projectID, environmentID), workspaceID: uuid.NewV7(), processID: processID},
		{name: "process", principal: workspaceExecHTTPPrincipal(orgID, projectID, environmentID), workspaceID: workspaceID, processID: uuid.NewV7()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.getWorkspaceExecHTTP(recorder, workspaceExecHTTPGetRequest(test.workspaceID.String(), test.processID.String(), test.principal))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if got := decodeHTTPError(t, recorder.Body.Bytes()).Code; got != "workspace_exec_not_found" {
				t.Fatalf("error code = %q", got)
			}
		})
	}
}

func workspaceExecHTTPPrincipal(orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID) auth.Actor {
	return auth.Actor{
		OrgID: orgID, APIKeyID: uuid.NewV7(), Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: projectID.String(), EnvironmentID: environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionWorkspaceExecCreate},
	}
}

func workspaceExecHTTPGetRequest(
	workspaceID string,
	processID string,
	principal auth.Actor,
) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("workspaceID", workspaceID)
	route.URLParams.Add("processID", processID)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}
