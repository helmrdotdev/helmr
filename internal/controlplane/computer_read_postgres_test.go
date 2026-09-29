package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestComputerReadPostgresListsAttachedAndIdleComputers(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	key := "owner:session"
	_, err := fixture.server.startActor(t.Context(), fixture.request(0, &key, "owner-session"))
	if err != nil {
		t.Fatal(err)
	}
	owned := fixture.computerIDs[0].String()
	free := fixture.computerIDs[1].String()
	principal := auth.Principal{
		OrgID: fixture.orgID, Kind: auth.PrincipalKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionComputersRead},
	}

	listRecorder := httptest.NewRecorder()
	fixture.server.listComputersHTTP(listRecorder, computerReadPostgresRequest("/v1/computers", "", principal))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list HTTP = %d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var list api.ListComputersResponse
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Computers) != 2 {
		t.Fatalf("computers = %+v", list)
	}
	for _, item := range list.Computers {
		if item.ID != owned && item.ID != free {
			t.Fatalf("unexpected Computer %+v", item)
		}
	}

	for _, id := range []string{owned, free} {
		recorder := httptest.NewRecorder()
		fixture.server.getComputerHTTP(recorder, computerReadPostgresRequest("/v1/computers/"+id, id, principal))
		if recorder.Code != http.StatusOK {
			t.Fatalf("get %s HTTP = %d body=%s", id, recorder.Code, recorder.Body.String())
		}
		var snapshot api.ComputerSnapshot
		if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.ID != id {
			t.Fatalf("snapshot %s = %+v", id, snapshot)
		}
	}
}

func computerReadPostgresRequest(target string, computerID string, principal auth.Principal) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	route := chi.NewRouteContext()
	if computerID != "" {
		route.URLParams.Add("computerID", computerID)
	}
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, principalContextKey{}, principal)
	return request.WithContext(ctx)
}
