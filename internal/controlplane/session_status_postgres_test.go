package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestActorReadPostgresProjectsStableStatus(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	key := "thread:read"
	request := fixture.request(0, &key, "read-status")
	result, err := fixture.server.startActor(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}

	failedAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE sessions
		   SET state = 'failed',
		       current_run_id = NULL,
		       failure = jsonb_build_object(
		           'code', 'run_failed',
		           'message', 'Session run failed',
		           'details', jsonb_build_object('run_id', ($1::uuid)::text)
		       ),
		       failure_run_id = $1::uuid,
		       failed_at = $2,
		       updated_at = $2
		 WHERE id = $3
	`, result.BootRunID, failedAt, result.SessionID); err != nil {
		t.Fatal(err)
	}

	principal := auth.Actor{
		OrgID: fixture.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionSessionsRead},
	}
	statusRequest := sessionReadPostgresRequest("/v1/sessions/"+result.SessionID.String(), result.SessionID.String(), principal)
	statusRecorder := httptest.NewRecorder()
	fixture.server.getSessionHTTP(statusRecorder, statusRequest)
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status HTTP = %d body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var status api.Session
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.ID != result.SessionID.String() ||
		status.Status != api.SessionStatusFailed ||
		status.Failure == nil ||
		status.Failure.Details.RunID != result.BootRunID.String() ||
		status.CurrentRunID != nil {
		t.Fatalf("status HTTP response = %+v", status)
	}
}

func sessionReadPostgresRequest(target string, sessionID string, principal auth.Actor) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("sessionID", sessionID)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}
