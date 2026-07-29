package control

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
		UPDATE actors
		   SET state = 'failed',
		       current_run_id = NULL,
		       failure_code = 'run_failed',
		       failure_run_id = $1,
		       failed_at = $2,
		       updated_at = $2
		 WHERE id = $3
	`, result.BootRunID, failedAt, result.ActorID); err != nil {
		t.Fatal(err)
	}

	principal := auth.Actor{
		OrgID: fixture.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionActorsRead},
	}
	statusRequest := actorReadPostgresRequest("/?actor_key=thread%3Aread", principal)
	statusRecorder := httptest.NewRecorder()
	fixture.server.getActorStatusHTTP(statusRecorder, statusRequest)
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status HTTP = %d body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var status api.ActorStatus
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.ID != result.ActorID.String() ||
		status.Status != api.ActorPublicStatusFailed ||
		status.Failure == nil ||
		status.Failure.RunID != result.BootRunID.String() ||
		status.CurrentRunID != nil {
		t.Fatalf("status HTTP response = %+v", status)
	}
}

func actorReadPostgresRequest(target string, principal auth.Actor) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("actorDeclaredID", "operator.v1")
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}
