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

func TestWorkspaceReadPostgresProjectsSessionOwner(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	key := "owner:session"
	started, err := fixture.server.startActor(t.Context(), fixture.request(0, &key, "owner-session"))
	if err != nil {
		t.Fatal(err)
	}
	owned := fixture.workspaceIDs[0].String()
	free := fixture.workspaceIDs[1].String()
	principal := auth.Actor{
		OrgID: fixture.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionWorkspacesRead},
	}
	wantOwner := &api.WorkspaceOwner{SessionID: started.SessionID.String()}

	listRecorder := httptest.NewRecorder()
	fixture.server.listWorkspacesHTTP(listRecorder, workspaceReadPostgresRequest("/v1/workspaces", "", principal))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list HTTP = %d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var list api.ListWorkspacesResponse
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Workspaces) != 2 {
		t.Fatalf("workspaces = %+v", list)
	}
	for _, item := range list.Workspaces {
		switch item.ID {
		case owned:
			if item.Owner == nil || *item.Owner != *wantOwner {
				t.Fatalf("owned Workspace = %+v", item)
			}
		case free:
			if item.Owner != nil {
				t.Fatalf("free Workspace = %+v", item)
			}
		default:
			t.Fatalf("unexpected Workspace %+v", item)
		}
	}

	for _, test := range []struct {
		id    string
		owner *api.WorkspaceOwner
	}{{owned, wantOwner}, {free, nil}} {
		recorder := httptest.NewRecorder()
		fixture.server.getWorkspaceHTTP(recorder, workspaceReadPostgresRequest("/v1/workspaces/"+test.id, test.id, principal))
		if recorder.Code != http.StatusOK {
			t.Fatalf("get %s HTTP = %d body=%s", test.id, recorder.Code, recorder.Body.String())
		}
		var snapshot api.WorkspaceSnapshot
		if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.ID != test.id || (snapshot.Owner == nil) != (test.owner == nil) ||
			(test.owner != nil && *snapshot.Owner != *test.owner) {
			t.Fatalf("snapshot %s = %+v", test.id, snapshot)
		}
	}
}

func workspaceReadPostgresRequest(target string, workspaceID string, principal auth.Actor) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	route := chi.NewRouteContext()
	if workspaceID != "" {
		route.URLParams.Add("workspaceID", workspaceID)
	}
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}
