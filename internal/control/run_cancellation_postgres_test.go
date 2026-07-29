package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
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
	var snapshot struct {
		ID                   string `json:"id"`
		Status               string `json:"status"`
		TerminalReasonCode   string `json:"terminal_reason_code"`
		CurrentAttemptNumber int32  `json:"current_attempt_number"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ID != started.BootRunID.String() || snapshot.Status != "cancelled" ||
		snapshot.TerminalReasonCode != "run_cancelled" ||
		snapshot.CurrentAttemptNumber != 1 {
		t.Fatalf("cancel snapshot = %+v", snapshot)
	}

	var actorState string
	var currentRunID *uuid.UUID
	var manualRunCancelled bool
	var ownerActorID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `
SELECT actors.state,
       actors.current_run_id,
       actors.manual_run_cancelled,
       workspaces.owner_actor_id
  FROM actors
  JOIN workspaces ON workspaces.id = actors.workspace_id
 WHERE actors.id = $1`, started.ActorID,
	).Scan(&actorState, &currentRunID, &manualRunCancelled, &ownerActorID); err != nil {
		t.Fatal(err)
	}
	if actorState != "open" || currentRunID != nil || !manualRunCancelled ||
		ownerActorID != started.ActorID {
		t.Fatalf(
			"Actor after Run cancellation = state:%s current:%v hold:%v owner:%s",
			actorState, currentRunID, manualRunCancelled, ownerActorID,
		)
	}

	if _, err := fixture.server.appendActorInput(t.Context(), appendActorInputRequest{
		EnvironmentID: fixture.environmentID,
		ActorID:       started.ActorID,
		RecordID:      uuid.Must(uuid.NewV7()),
		Data:          json.RawMessage(`{"message":"continue"}`),
		SourceKind:    "external",
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(t.Context(), `
SELECT current_run_id, manual_run_cancelled
  FROM actors
 WHERE id = $1`, started.ActorID,
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
	request := httptest.NewRequest(http.MethodPost, "/api/runs/not-a-run/cancel", nil)
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
	var response struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
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
	request := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/cancel", nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("runID", runID)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}
