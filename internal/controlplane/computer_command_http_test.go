package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type computerCommandHTTPStore struct {
	db.Querier
	want  db.GetCommandParams
	value db.ComputerCommand
	calls int
}

func (s *computerCommandHTTPStore) GetCommand(
	_ context.Context,
	params db.GetCommandParams,
) (db.ComputerCommand, error) {
	s.calls++
	if params != s.want {
		return db.ComputerCommand{}, pgx.ErrNoRows
	}
	return s.value, nil
}

func TestGetComputerCommandHTTPProjectsEveryPublicState(t *testing.T) {
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	computerID := uuid.NewV7()
	commandID := uuid.NewV7()
	principal := computerCommandHTTPPrincipal(orgID, projectID, environmentID)
	want := db.GetCommandParams{
		OrgID: pgvalue.UUID(orgID), ProjectID: pgvalue.UUID(projectID),
		EnvironmentID: pgvalue.UUID(environmentID),
		CommandID:     pgvalue.UUID(commandID),
	}

	tests := []struct {
		name       string
		process    db.ComputerCommand
		statusCode int
		status     string
	}{
		{name: "pending", process: db.ComputerCommand{ID: pgvalue.UUID(commandID), Status: db.ComputerCommandStatusPending}, statusCode: http.StatusOK, status: "pending"},
		{name: "starting", process: db.ComputerCommand{ID: pgvalue.UUID(commandID), Status: db.ComputerCommandStatusStarting}, statusCode: http.StatusOK, status: "starting"},
		{name: "running", process: db.ComputerCommand{ID: pgvalue.UUID(commandID), Status: db.ComputerCommandStatusRunning}, statusCode: http.StatusOK, status: "running"},
		{name: "stopping", process: db.ComputerCommand{ID: pgvalue.UUID(commandID), Status: db.ComputerCommandStatusStopping}, statusCode: http.StatusOK, status: "stopping"},
		{name: "exited", process: db.ComputerCommand{
			ID: pgvalue.UUID(commandID), Status: db.ComputerCommandStatusExited,
			ExitCode: pgtype.Int4{Int32: 17, Valid: true},
		}, statusCode: http.StatusOK, status: "exited"},
		{name: "timed out", process: db.ComputerCommand{ID: pgvalue.UUID(commandID), Status: db.ComputerCommandStatusTimedOut}, statusCode: http.StatusOK, status: "timed_out"},
		{name: "cancelled", process: db.ComputerCommand{ID: pgvalue.UUID(commandID), Status: db.ComputerCommandStatusCancelled}, statusCode: http.StatusOK, status: "cancelled"},
		{name: "lost", process: db.ComputerCommand{ID: pgvalue.UUID(commandID), Status: db.ComputerCommandStatusLost, FailureReason: pgvalue.Text("guest_failure")}, statusCode: http.StatusOK, status: "lost"},
		{name: "failed", process: db.ComputerCommand{
			ID: pgvalue.UUID(commandID), Status: db.ComputerCommandStatusFailed,
			FailureReason: pgvalue.Text("placement_failed"),
		}, statusCode: http.StatusOK, status: "failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.process.ComputerID = pgvalue.UUID(computerID)
			if computerCommandTerminal(test.process.Status) {
				test.process.TerminalAt = pgvalue.Timestamptz(time.Now())
			}
			store := &computerCommandHTTPStore{want: want, value: test.process}
			recorder := httptest.NewRecorder()
			(&Server{db: store}).getComputerCommandHTTP(
				recorder,
				computerCommandHTTPGetRequest(commandID.String(), principal),
			)
			if recorder.Code != test.statusCode {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var response api.CommandInfo
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.ID != commandID.String() || response.Status != test.status {
				t.Fatalf("response = %+v", response)
			}
			if test.status == "exited" && (response.Outcome == nil || response.Outcome.ExitCode == nil || *response.Outcome.ExitCode != 17) {
				t.Fatalf("exited response = %+v", response)
			}
			if test.status == "failed" && (response.Outcome == nil || response.Outcome.Failure == nil || response.Outcome.Failure.Reason != "placement_failed") {
				t.Fatalf("failed response = %+v", response)
			}
		})
	}
}

func TestGetComputerCommandHTTPReportsPrunedResult(t *testing.T) {
	orgID, projectID, environmentID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	commandID := uuid.NewV7()
	store := &computerCommandHTTPStore{
		want:  db.GetCommandParams{OrgID: pgvalue.UUID(orgID), ProjectID: pgvalue.UUID(projectID), EnvironmentID: pgvalue.UUID(environmentID), CommandID: pgvalue.UUID(commandID)},
		value: db.ComputerCommand{ID: pgvalue.UUID(commandID), Status: db.ComputerCommandStatusFailed, ResultPrunedAt: pgvalue.Timestamptz(time.Now())},
	}
	recorder := httptest.NewRecorder()
	(&Server{db: store}).getComputerCommandHTTP(recorder, computerCommandHTTPGetRequest(commandID.String(), computerCommandHTTPPrincipal(orgID, projectID, environmentID)))
	if recorder.Code != http.StatusGone || !strings.Contains(recorder.Body.String(), "command_result_expired") {
		t.Fatalf("pruned Command HTTP=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestGetComputerCommandHTTPRequiresCommandPermissionAndValidIDs(t *testing.T) {
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	computerID := uuid.NewV7()
	commandID := uuid.NewV7()
	store := &computerCommandHTTPStore{}
	server := &Server{db: store}

	viewer := auth.Principal{
		OrgID: orgID, Kind: auth.PrincipalKindAPIKey, Role: auth.RoleViewer,
		ProjectID: projectID.String(), EnvironmentID: environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionComputersRead},
	}
	recorder := httptest.NewRecorder()
	server.getComputerCommandHTTP(recorder, computerCommandHTTPGetRequest(commandID.String(), viewer))
	if recorder.Code != http.StatusForbidden || store.calls != 0 {
		t.Fatalf("viewer status=%d calls=%d body=%s", recorder.Code, store.calls, recorder.Body.String())
	}

	principal := computerCommandHTTPPrincipal(orgID, projectID, environmentID)
	for _, test := range []struct {
		name       string
		computerID string
		commandID  string
	}{
		{name: "process", computerID: computerID.String(), commandID: "not-a-process"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.getComputerCommandHTTP(recorder, computerCommandHTTPGetRequest(test.commandID, principal))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if store.calls != 0 {
		t.Fatalf("invalid requests reached store %d times", store.calls)
	}
}

func TestGetComputerCommandHTTPIsolatesEveryAuthorityCoordinate(t *testing.T) {
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	computerID := uuid.NewV7()
	commandID := uuid.NewV7()
	store := &computerCommandHTTPStore{
		want: db.GetCommandParams{
			OrgID: pgvalue.UUID(orgID), ProjectID: pgvalue.UUID(projectID),
			EnvironmentID: pgvalue.UUID(environmentID),
			CommandID:     pgvalue.UUID(commandID),
		},
		value: db.ComputerCommand{ID: pgvalue.UUID(commandID), Status: db.ComputerCommandStatusPending},
	}
	server := &Server{db: store}

	tests := []struct {
		name       string
		principal  auth.Principal
		computerID uuid.UUID
		commandID  uuid.UUID
	}{
		{name: "organization", principal: computerCommandHTTPPrincipal(uuid.NewV7(), projectID, environmentID), computerID: computerID, commandID: commandID},
		{name: "project", principal: computerCommandHTTPPrincipal(orgID, uuid.NewV7(), environmentID), computerID: computerID, commandID: commandID},
		{name: "environment", principal: computerCommandHTTPPrincipal(orgID, projectID, uuid.NewV7()), computerID: computerID, commandID: commandID},
		{name: "process", principal: computerCommandHTTPPrincipal(orgID, projectID, environmentID), computerID: computerID, commandID: uuid.NewV7()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.getComputerCommandHTTP(recorder, computerCommandHTTPGetRequest(test.commandID.String(), test.principal))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if got := decodeHTTPError(t, recorder.Body.Bytes()).Code; got != "computer_command_not_found" {
				t.Fatalf("error code = %q", got)
			}
		})
	}
}

func computerCommandHTTPPrincipal(orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID) auth.Principal {
	return auth.Principal{
		OrgID: orgID, APIKeyID: uuid.NewV7(), Kind: auth.PrincipalKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: projectID.String(), EnvironmentID: environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionComputerCommandCreate},
	}
}

func computerCommandHTTPGetRequest(
	commandID string,
	principal auth.Principal,
) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("commandID", commandID)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, principalContextKey{}, principal)
	return request.WithContext(ctx)
}
