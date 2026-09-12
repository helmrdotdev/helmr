package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestSessionListPostgresFiltersByPublicStatus(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 3)
	keys := []string{"list:open", "list:closing", "list:failed"}
	sessions := make([]string, len(keys))
	var failedRunID string
	for index, key := range keys {
		result, err := fixture.server.startActor(t.Context(), fixture.request(index, &keys[index], "list-"+key))
		if err != nil {
			t.Fatal(err)
		}
		sessions[index] = result.SessionID.String()
		if index == 2 {
			failedRunID = result.BootRunID.String()
		}
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE sessions
		   SET state = 'closing',
		       updated_at = now()
		 WHERE id = $1
	`, sessions[1]); err != nil {
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
	`, failedRunID, failedAt, sessions[2]); err != nil {
		t.Fatal(err)
	}

	principal := auth.Actor{
		OrgID: fixture.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionSessionsRead},
	}

	all := listSessionsPostgresHTTP(t, fixture, principal, "/v1/sessions")
	if sessionListIDs(all) != sessions[2]+","+sessions[1]+","+sessions[0] {
		t.Fatalf("unfiltered Sessions = %+v", all)
	}
	open := listSessionsPostgresHTTP(t, fixture, principal, "/v1/sessions?status=open")
	if sessionListIDs(open) != sessions[1]+","+sessions[0] ||
		open.Sessions[0].Status != api.SessionStatusOpen || open.Sessions[1].Status != api.SessionStatusOpen {
		t.Fatalf("open Sessions = %+v", open)
	}
	failed := listSessionsPostgresHTTP(t, fixture, principal, "/v1/sessions?status=failed")
	if sessionListIDs(failed) != sessions[2] || failed.Sessions[0].Status != api.SessionStatusFailed ||
		failed.Sessions[0].Failure == nil || failed.Sessions[0].Failure.Details.RunID != failedRunID {
		t.Fatalf("failed Sessions = %+v", failed)
	}
	if closed := listSessionsPostgresHTTP(
		t, fixture, principal, "/v1/sessions?status=closed&status=cancelled",
	); len(closed.Sessions) != 0 || closed.NextCursor != "" {
		t.Fatalf("closed Sessions = %+v", closed)
	}
	both := listSessionsPostgresHTTP(t, fixture, principal, "/v1/sessions?status=open&status=failed")
	if sessionListIDs(both) != sessions[2]+","+sessions[1]+","+sessions[0] {
		t.Fatalf("open and failed Sessions = %+v", both)
	}

	page := listSessionsPostgresHTTP(t, fixture, principal, "/v1/sessions?status=open&limit=1")
	if sessionListIDs(page) != sessions[1] || page.NextCursor == "" {
		t.Fatalf("first page = %+v", page)
	}
	next := listSessionsPostgresHTTP(
		t, fixture, principal, "/v1/sessions?status=open&limit=1&cursor="+page.NextCursor,
	)
	if sessionListIDs(next) != sessions[0] || next.NextCursor != "" {
		t.Fatalf("next page = %+v", next)
	}
	for _, target := range []string{
		"/v1/sessions?status=failed&limit=1&cursor=" + page.NextCursor,
		"/v1/sessions?limit=1&cursor=" + page.NextCursor,
		"/v1/sessions?status=closing",
		"/v1/sessions?actor_id=operator.v1&key=" + url.QueryEscape(keys[1]) + "&status=open",
	} {
		recorder := httptest.NewRecorder()
		fixture.server.listSessionsHTTP(recorder, sessionReadPostgresRequest(target, "", principal))
		if recorder.Code != http.StatusBadRequest ||
			!strings.Contains(recorder.Body.String(), `"code":"invalid_session_query"`) {
			t.Fatalf("%s response = %d %s", target, recorder.Code, recorder.Body.String())
		}
	}

	exact := listSessionsPostgresHTTP(
		t, fixture, principal, "/v1/sessions?actor_id=operator.v1&key="+url.QueryEscape(keys[1]),
	)
	if sessionListIDs(exact) != sessions[1] || exact.Sessions[0].Status != api.SessionStatusOpen ||
		exact.Sessions[0].WorkspaceID != fixture.workspaceIDs[1].String() || exact.NextCursor != "" {
		t.Fatalf("exact lookup = %+v", exact)
	}
	for index, item := range all.Sessions {
		if item.WorkspaceID != fixture.workspaceIDs[len(all.Sessions)-1-index].String() {
			t.Fatalf("Session %s Workspace = %q", item.ID, item.WorkspaceID)
		}
	}
}

func sessionListIDs(response api.ListSessionsResponse) string {
	ids := make([]string, 0, len(response.Sessions))
	for _, item := range response.Sessions {
		ids = append(ids, item.ID)
	}
	return strings.Join(ids, ",")
}

func listSessionsPostgresHTTP(
	t *testing.T,
	fixture actorStartPostgresFixture,
	principal auth.Actor,
	target string,
) api.ListSessionsResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	fixture.server.listSessionsHTTP(recorder, sessionReadPostgresRequest(target, "", principal))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s HTTP = %d body=%s", target, recorder.Code, recorder.Body.String())
	}
	var response api.ListSessionsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}
