package controlplane

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestActorReadPostgresProjectsStableStatus(t *testing.T) {
	fixture := newSessionHTTP(t, sessiontest.New(t, 1))
	key := "thread:read"
	result := startSession(t, fixture.Fixture, 0, &key, "read-status")

	failedAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := fixture.Pool.Exec(t.Context(), `
		UPDATE sessions
		   SET status = 'failed',
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

	token := fixture.apiKey(auth.Principal{
		OrgID: fixture.OrgID, Kind: auth.PrincipalKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.ProjectID.String(), EnvironmentID: fixture.EnvironmentID.String(),
		Permissions: []auth.Permission{auth.PermissionSessionsRead},
	})
	statusRecorder := fixture.request(t, http.MethodGet, "/v1/sessions/"+result.SessionID.String(), token, "")
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status HTTP = %d body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var status api.Session
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.ID != result.SessionID.String() ||
		status.ComputerID != fixture.ComputerIDs[0].String() ||
		status.Status != api.SessionStatusFailed ||
		status.Failure == nil ||
		status.Failure.Details.RunID != result.BootRunID.String() ||
		status.CurrentRunID != nil {
		t.Fatalf("status HTTP response = %+v", status)
	}
	missing := fixture.request(t, http.MethodGet, "/v1/sessions/"+uuid.NewV7().String(), token, "")
	if missing.Code != http.StatusNotFound || decodeHTTPError(t, missing.Body.Bytes()).Message != "session was not found" {
		t.Fatalf("missing Session HTTP = %d body=%s", missing.Code, missing.Body.String())
	}
}
