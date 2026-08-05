package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
)

func TestCancelRunHTTPInstallsActorHoldAndInputClearsIt(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	started, err := fixture.server.startActor(t.Context(), fixture.request(0, nil, "actor-cancel"))
	if err != nil {
		t.Fatal(err)
	}
	principal := auth.Actor{
		OrgID: fixture.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleOwner,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionRunsManage, auth.PermissionRunsRead},
	}

	recorder := httptest.NewRecorder()
	fixture.server.cancelRunHTTP(
		recorder,
		runCancellationRequest(t, started.BootRunID.String(), principal),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var snapshot api.RunSnapshotResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ID != started.BootRunID.String() || snapshot.Status != "cancelled" ||
		snapshot.Failure == nil || snapshot.Failure.Code != "run_cancelled" ||
		snapshot.CurrentAttemptNumber != 1 {
		t.Fatalf("cancel snapshot = %+v", snapshot)
	}

	var actorState string
	var currentRunID *uuid.UUID
	var manualRunCancelled bool
	var ownerSessionID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `
SELECT sessions.state,
       sessions.current_run_id,
       sessions.manual_run_cancelled,
       workspaces.owner_session_id
  FROM sessions
  JOIN workspaces ON workspaces.id = sessions.workspace_id
 WHERE sessions.id = $1`, started.SessionID,
	).Scan(&actorState, &currentRunID, &manualRunCancelled, &ownerSessionID); err != nil {
		t.Fatal(err)
	}
	if actorState != "open" || currentRunID != nil || !manualRunCancelled ||
		ownerSessionID != started.SessionID {
		t.Fatalf(
			"Actor after Run cancellation = state:%s current:%v hold:%v owner:%s",
			actorState, currentRunID, manualRunCancelled, ownerSessionID,
		)
	}

	if _, err := fixture.server.appendActorInput(t.Context(), appendActorInputRequest{
		EnvironmentID: fixture.environmentID,
		SessionID:     started.SessionID,
		RecordID:      uuid.Must(uuid.NewV7()),
		Data:          json.RawMessage(`{"message":"continue"}`),
		SourceKind:    "external",
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(t.Context(), `
SELECT current_run_id, manual_run_cancelled
  FROM sessions
 WHERE id = $1`, started.SessionID,
	).Scan(&currentRunID, &manualRunCancelled); err != nil {
		t.Fatal(err)
	}
	if currentRunID == nil || *currentRunID == started.BootRunID || manualRunCancelled {
		t.Fatalf("Actor successor = current:%v hold:%v", currentRunID, manualRunCancelled)
	}
	var successorStatus db.RunStatus
	if err := fixture.pool.QueryRow(t.Context(),
		`SELECT status FROM runs WHERE id = $1`, *currentRunID,
	).Scan(&successorStatus); err != nil {
		t.Fatal(err)
	}
	if successorStatus != db.RunStatusQueued {
		t.Fatalf("Actor successor status = %s", successorStatus)
	}
}

func TestCancelRunHTTPDeniesBeforeRunValidation(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/runs/not-a-run/cancel", nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("runID", "not-a-run")
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, auth.Actor{
		Kind: auth.ActorKindAPIKey, OrgID: uuid.Must(uuid.NewV7()),
		ProjectID: uuid.Must(uuid.NewV7()).String(), EnvironmentID: uuid.Must(uuid.NewV7()).String(),
	})
	recorder := httptest.NewRecorder()

	(&Server{}).cancelRunHTTP(recorder, request.WithContext(ctx))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeHTTPError(t, recorder.Body.Bytes())
	if response.Code != "permission_required" {
		t.Fatalf("unexpected response: %s", recorder.Body.String())
	}
}

func runCancellationRequest(
	t *testing.T,
	runID string,
	principal auth.Actor,
) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/runs/"+runID+"/cancel", nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("runID", runID)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}
