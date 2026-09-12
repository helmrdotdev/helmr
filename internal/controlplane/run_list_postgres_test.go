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
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRunListPostgresFiltersBySession(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	firstKey, secondKey := "runs:first", "runs:second"
	first, err := fixture.server.startActor(t.Context(), fixture.request(0, &firstKey, "runs-first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.server.startActor(t.Context(), fixture.request(1, &secondKey, "runs-second"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE runs
		   SET status = 'succeeded',
		       output = 'null'::jsonb,
		       terminal_at = transaction_timestamp(),
		       updated_at = transaction_timestamp()
		 WHERE id = $1
	`, first.BootRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE sessions
		   SET current_run_id = NULL,
		       next_input_sequence = 2
		 WHERE id = $1
	`, first.SessionID); err != nil {
		t.Fatal(err)
	}
	continuationID := uuid.NewV7()
	if _, err := db.New(fixture.pool).CreateActorContinuationRun(t.Context(), db.CreateActorContinuationRunParams{
		RunID:                 pgvalue.UUID(continuationID),
		QueueOriginAt:         pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		RootSpanID:            "0000000000000001",
		EnvironmentID:         pgvalue.UUID(fixture.environmentID),
		SessionID:             pgvalue.UUID(first.SessionID),
		WorkspaceID:           pgvalue.UUID(fixture.workspaceIDs[0]),
		ExpectedRunGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}

	principal := auth.Actor{
		OrgID: fixture.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionRunsRead},
	}
	firstSession := first.SessionID.String()
	secondSession := second.SessionID.String()

	all := listRunsPostgresHTTP(t, fixture, principal, "/v1/runs")
	if runListIDs(all) != strings.Join([]string{
		continuationID.String(), second.BootRunID.String(), first.BootRunID.String(),
	}, ",") || all.NextCursor != "" {
		t.Fatalf("unfiltered runs = %+v", all)
	}

	firstRuns := listRunsPostgresHTTP(t, fixture, principal, "/v1/runs?session_id="+firstSession)
	if runListIDs(firstRuns) != continuationID.String()+","+first.BootRunID.String() {
		t.Fatalf("first Session runs = %+v", firstRuns)
	}
	for _, item := range firstRuns.Runs {
		if item.SessionID != firstSession {
			t.Fatalf("run %s belongs to Session %q", item.ID, item.SessionID)
		}
	}

	succeeded := listRunsPostgresHTTP(
		t, fixture, principal, "/v1/runs?status=succeeded&session_id="+firstSession,
	)
	if runListIDs(succeeded) != first.BootRunID.String() || succeeded.Runs[0].Status != api.RunStatusSucceeded {
		t.Fatalf("succeeded first Session runs = %+v", succeeded)
	}
	queuedSecond := listRunsPostgresHTTP(
		t, fixture, principal, "/v1/runs?session_id="+secondSession+"&status=queued",
	)
	if runListIDs(queuedSecond) != second.BootRunID.String() {
		t.Fatalf("queued second Session runs = %+v", queuedSecond)
	}
	if none := listRunsPostgresHTTP(
		t, fixture, principal, "/v1/runs?session_id="+uuid.NewV7().String(),
	); len(none.Runs) != 0 || none.NextCursor != "" {
		t.Fatalf("unknown Session runs = %+v", none)
	}

	page := listRunsPostgresHTTP(t, fixture, principal, "/v1/runs?session_id="+firstSession+"&limit=1")
	if runListIDs(page) != continuationID.String() || page.NextCursor == "" {
		t.Fatalf("first page = %+v", page)
	}
	next := listRunsPostgresHTTP(
		t, fixture, principal, "/v1/runs?session_id="+firstSession+"&limit=1&cursor="+page.NextCursor,
	)
	if runListIDs(next) != first.BootRunID.String() || next.NextCursor != "" {
		t.Fatalf("next page = %+v", next)
	}
	for _, target := range []string{
		"/v1/runs?session_id=" + secondSession + "&limit=1&cursor=" + page.NextCursor,
		"/v1/runs?limit=1&cursor=" + page.NextCursor,
		"/v1/runs?session_id=" + firstSession + "&status=queued&limit=1&cursor=" + page.NextCursor,
	} {
		recorder := httptest.NewRecorder()
		fixture.server.listRunSnapshotsHTTP(recorder, runListPostgresRequest(target, principal))
		if recorder.Code != http.StatusBadRequest ||
			!strings.Contains(recorder.Body.String(), `"code":"invalid_run_cursor"`) {
			t.Fatalf("%s response = %d %s", target, recorder.Code, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	fixture.server.listRunSnapshotsHTTP(recorder, runListPostgresRequest("/v1/runs?session_id=nope", principal))
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), `"code":"invalid_run_list"`) {
		t.Fatalf("invalid session_id response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func runListIDs(response api.ListRunsResponse) string {
	ids := make([]string, 0, len(response.Runs))
	for _, item := range response.Runs {
		ids = append(ids, item.ID)
	}
	return strings.Join(ids, ",")
}

func runListPostgresRequest(target string, principal auth.Actor) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, chi.NewRouteContext())
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}

func listRunsPostgresHTTP(
	t *testing.T,
	fixture actorStartPostgresFixture,
	principal auth.Actor,
	target string,
) api.ListRunsResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	fixture.server.listRunSnapshotsHTTP(recorder, runListPostgresRequest(target, principal))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s HTTP = %d body=%s", target, recorder.Code, recorder.Body.String())
	}
	var response api.ListRunsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}
