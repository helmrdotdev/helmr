package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestResourceReadsPreserveUnknownDiagnostics(t *testing.T) {
	scheduleFailure := &api.ScheduleFailure{Code: "future_schedule_failure", Message: "diagnosis", Details: json.RawMessage(`{"custom":1}`)}
	session := actorStatusFixture()
	session.Status = api.SessionStatusFailed
	session.Failure = &api.SessionFailure{Code: "future_session_failure", Message: "diagnosis", Details: api.SessionFailureDetails{RunID: session.WorkspaceID}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/schedules/"+testScheduleID {
			_ = json.NewEncoder(w).Encode(api.ScheduleResponse{ID: testScheduleID, TaskID: "nightly", Status: api.ScheduleStatusErrored, LastFailure: scheduleFailure})
		} else if r.URL.Path == "/v1/sessions/"+testSessionID {
			_ = json.NewEncoder(w).Encode(session)
		} else {
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := client.GetSchedule(t.Context(), testScheduleID, EnvironmentScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(schedule.LastFailure, scheduleFailure) {
		t.Fatalf("schedule failure=%+v", schedule.LastFailure)
	}
	got, err := client.RetrieveSession(t.Context(), testSessionID, EnvironmentScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Failure, session.Failure) {
		t.Fatalf("session failure=%+v", got.Failure)
	}
}
