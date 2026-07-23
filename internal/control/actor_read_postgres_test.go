package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestActorReadPostgresProjectsJoinedRunsAndStablePages(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 3)
	var actorListIndex string
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT indexdef
		  FROM pg_indexes
		 WHERE schemaname = current_schema()
		   AND tablename = 'actors'
		   AND indexname = 'actors_list_idx'
	`).Scan(&actorListIndex); err != nil {
		t.Fatal(err)
	}
	if actorListIndex == "" {
		t.Fatal("actors_list_idx is missing")
	}
	results := make([]actorStartResult, 0, 3)
	for index, key := range []string{"thread:a", "thread:b", "thread:c"} {
		request := fixture.request(index, &key, "read-"+key)
		request.Metadata = []byte(`{"owner":"read-test"}`)
		request.Tags = []string{"actor"}
		request.ManagedRunMetadata = []byte(`{"kind":"managed"}`)
		request.ManagedRunTags = []string{"run"}
		result, err := fixture.server.startActor(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}

	createdAt := time.Date(2030, 1, 2, 3, 4, 5, 123456000, time.UTC)
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE actors SET created_at = $1, updated_at = $1 WHERE environment_id = $2
	`, createdAt, fixture.environmentID); err != nil {
		t.Fatal(err)
	}
	failed := results[1]
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE actors
		   SET state = 'failed',
		       current_run_id = NULL,
		       failure_code = 'run-failed',
		       failure_run_id = $1,
		       failed_at = $2,
		       updated_at = $2
		 WHERE id = $3
	`, failed.BootRunID, createdAt.Add(time.Minute), failed.ActorID); err != nil {
		t.Fatal(err)
	}

	queries := db.New(fixture.pool)
	byID, err := queries.GetActorRead(t.Context(), db.GetActorReadParams{
		EnvironmentID:   pgvalue.UUID(fixture.environmentID),
		ActorDeclaredID: "operator.v1",
		AddressPublicID: pgvalue.Text(failed.ActorPublicID),
	})
	if err != nil {
		t.Fatal(err)
	}
	failedStatus, err := projectActorStatus(actorReadRecordFromGet(byID))
	if err != nil {
		t.Fatal(err)
	}
	if failedStatus.Failure == nil ||
		failedStatus.Failure.Code != "run-failed" ||
		failedStatus.Failure.RunID != failed.BootRunPublicID ||
		failedStatus.CurrentRunID != nil {
		t.Fatalf("failed status = %+v", failedStatus)
	}

	byKey, err := queries.GetActorRead(t.Context(), db.GetActorReadParams{
		EnvironmentID:   pgvalue.UUID(fixture.environmentID),
		ActorDeclaredID: "operator.v1",
		AddressKey:      pgvalue.Text("thread:a"),
	})
	if err != nil {
		t.Fatal(err)
	}
	openStatus, err := projectActorStatus(actorReadRecordFromGet(byKey))
	if err != nil {
		t.Fatal(err)
	}
	if openStatus.CurrentRunID == nil || *openStatus.CurrentRunID != results[0].BootRunPublicID {
		t.Fatalf("open status = %+v", openStatus)
	}

	expected := []string{
		results[0].ActorPublicID,
		results[1].ActorPublicID,
		results[2].ActorPublicID,
	}
	sort.Sort(sort.Reverse(sort.StringSlice(expected)))
	firstPage, err := queries.ListActorReads(t.Context(), db.ListActorReadsParams{
		EnvironmentID:   pgvalue.UUID(fixture.environmentID),
		ActorDeclaredID: "operator.v1",
		LimitCount:      2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstPage) != 2 ||
		firstPage[0].PublicID != expected[0] ||
		firstPage[1].PublicID != expected[1] {
		t.Fatalf("first page = %+v, want %v", actorReadPublicIDs(firstPage), expected[:2])
	}
	secondPage, err := queries.ListActorReads(t.Context(), db.ListActorReadsParams{
		EnvironmentID:   pgvalue.UUID(fixture.environmentID),
		ActorDeclaredID: "operator.v1",
		CursorCreatedAt: pgtype.Timestamptz{Time: firstPage[1].CreatedAt.Time, Valid: true},
		CursorPublicID:  pgvalue.Text(firstPage[1].PublicID),
		LimitCount:      2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(secondPage) != 1 || secondPage[0].PublicID != expected[2] {
		t.Fatalf("second page = %+v, want %q", actorReadPublicIDs(secondPage), expected[2])
	}
	failedOnly, err := queries.ListActorReads(t.Context(), db.ListActorReadsParams{
		EnvironmentID:   pgvalue.UUID(fixture.environmentID),
		ActorDeclaredID: "operator.v1",
		Lifecycle:       pgvalue.Text("failed"),
		LimitCount:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(failedOnly) != 1 || failedOnly[0].PublicID != failed.ActorPublicID {
		t.Fatalf("failed actors = %+v", actorReadPublicIDs(failedOnly))
	}

	principal := auth.Actor{
		OrgID: fixture.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionActorsRead},
	}
	statusRequest := actorReadPostgresRequest(
		"/?actor_key=thread%3Ab",
		principal,
	)
	statusRecorder := httptest.NewRecorder()
	fixture.server.getActorStatusHTTP(statusRecorder, statusRequest)
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status HTTP = %d body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var statusResponse api.ActorStatus
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &statusResponse); err != nil {
		t.Fatal(err)
	}
	if statusResponse.ID != failed.ActorPublicID ||
		statusResponse.Failure == nil ||
		statusResponse.Failure.RunID != failed.BootRunPublicID {
		t.Fatalf("status HTTP response = %+v", statusResponse)
	}

	listRequest := actorReadPostgresRequest("/?limit=2", principal)
	listRecorder := httptest.NewRecorder()
	fixture.server.listActorsHTTP(listRecorder, listRequest)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list HTTP = %d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var listResponse api.ListActorsResponse
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listResponse); err != nil {
		t.Fatal(err)
	}
	if len(listResponse.Actors) != 2 ||
		listResponse.Actors[0].ID != expected[0] ||
		listResponse.Actors[1].ID != expected[1] ||
		listResponse.NextCursor == "" {
		t.Fatalf("list HTTP response = %+v", listResponse)
	}
	nextRequest := actorReadPostgresRequest(
		"/?limit=2&cursor="+url.QueryEscape(listResponse.NextCursor),
		principal,
	)
	nextRecorder := httptest.NewRecorder()
	fixture.server.listActorsHTTP(nextRecorder, nextRequest)
	if nextRecorder.Code != http.StatusOK {
		t.Fatalf("next list HTTP = %d body=%s", nextRecorder.Code, nextRecorder.Body.String())
	}
	var nextResponse api.ListActorsResponse
	if err := json.Unmarshal(nextRecorder.Body.Bytes(), &nextResponse); err != nil {
		t.Fatal(err)
	}
	if len(nextResponse.Actors) != 1 ||
		nextResponse.Actors[0].ID != expected[2] ||
		nextResponse.NextCursor != "" {
		t.Fatalf("next list HTTP response = %+v", nextResponse)
	}

	failedRequest := actorReadPostgresRequest("/?lifecycle=failed&limit=1", principal)
	failedRecorder := httptest.NewRecorder()
	fixture.server.listActorsHTTP(failedRecorder, failedRequest)
	if failedRecorder.Code != http.StatusOK {
		t.Fatalf("failed list HTTP = %d body=%s", failedRecorder.Code, failedRecorder.Body.String())
	}
	var failedResponse api.ListActorsResponse
	if err := json.Unmarshal(failedRecorder.Body.Bytes(), &failedResponse); err != nil {
		t.Fatal(err)
	}
	if len(failedResponse.Actors) != 1 ||
		failedResponse.Actors[0].ID != failed.ActorPublicID ||
		failedResponse.NextCursor != "" {
		t.Fatalf("failed list HTTP response = %+v", failedResponse)
	}
}

func actorReadPublicIDs(rows []db.ListActorReadsRow) []string {
	ids := make([]string, len(rows))
	for index, row := range rows {
		ids[index] = row.PublicID
	}
	return ids
}

func actorReadPostgresRequest(target string, principal auth.Actor) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("actorDeclaredID", "operator.v1")
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}
