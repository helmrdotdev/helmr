package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestUnscopedSessionWorkspaceLookupUsesActualScope(t *testing.T) {
	workspace := actualScopeTestWorkspace()
	store := &fakeStore{workspace: workspace}
	server := &Server{db: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	recorder := httptest.NewRecorder()

	server.getWorkspace(recorder, actualScopeWorkspaceRequest(http.MethodGet, workspace, auth.Actor{
		OrgID: dbtest.DefaultOrgID,
		Role:  auth.RoleViewer,
		Kind:  auth.ActorKindSession,
	}, false, ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response api.WorkspaceEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Workspace.ProjectID != pgvalue.MustUUIDValue(workspace.ProjectID).String() ||
		response.Workspace.EnvironmentID != pgvalue.MustUUIDValue(workspace.EnvironmentID).String() {
		t.Fatalf("workspace scope = %s/%s", response.Workspace.ProjectID, response.Workspace.EnvironmentID)
	}
	if store.getWorkspaceByOrgAndIDCalls != 1 || store.getWorkspaceCalls != 0 {
		t.Fatalf("workspace lookups = actual:%d scoped:%d", store.getWorkspaceByOrgAndIDCalls, store.getWorkspaceCalls)
	}
}

func TestUnscopedSessionWorkspaceLookupHidesMissingCrossOrgAndUnauthorized(t *testing.T) {
	workspace := actualScopeTestWorkspace()
	cases := []struct {
		name      string
		actor     auth.Actor
		method    string
		workspace string
		handler   func(*Server, http.ResponseWriter, *http.Request)
	}{
		{
			name:      "missing",
			actor:     auth.Actor{OrgID: dbtest.DefaultOrgID, Role: auth.RoleViewer, Kind: auth.ActorKindSession},
			method:    http.MethodGet,
			workspace: "00000000-0000-0000-0000-000000000999",
			handler:   func(server *Server, w http.ResponseWriter, r *http.Request) { server.getWorkspace(w, r) },
		},
		{
			name:      "cross org",
			actor:     auth.Actor{OrgID: uuid.MustParse("00000000-0000-0000-0000-000000000099"), Role: auth.RoleViewer, Kind: auth.ActorKindSession},
			method:    http.MethodGet,
			workspace: pgvalue.MustUUIDValue(workspace.ID).String(),
			handler:   func(server *Server, w http.ResponseWriter, r *http.Request) { server.getWorkspace(w, r) },
		},
		{
			name:      "unauthorized",
			actor:     auth.Actor{OrgID: dbtest.DefaultOrgID, Role: auth.RoleViewer, Kind: auth.ActorKindSession},
			method:    http.MethodPatch,
			workspace: pgvalue.MustUUIDValue(workspace.ID).String(),
			handler:   func(server *Server, w http.ResponseWriter, r *http.Request) { server.patchWorkspace(w, r) },
		},
	}

	var notFoundBody string
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{workspace: workspace}
			server := &Server{db: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			recorder := httptest.NewRecorder()
			request := actualScopeWorkspaceRequest(test.method, workspace, test.actor, false, test.workspace)
			test.handler(server, recorder, request)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
			if notFoundBody == "" {
				notFoundBody = recorder.Body.String()
			} else if recorder.Body.String() != notFoundBody {
				t.Fatalf("body = %q, want indistinguishable %q", recorder.Body.String(), notFoundBody)
			}
		})
	}
}

func TestAPIKeyWorkspaceLookupRemainsFixedToKeyScope(t *testing.T) {
	workspace := actualScopeTestWorkspace()
	projectID := pgvalue.MustUUIDValue(workspace.ProjectID).String()
	environmentID := pgvalue.MustUUIDValue(workspace.EnvironmentID).String()

	t.Run("matching scope", func(t *testing.T) {
		store := &fakeStore{workspace: workspace}
		server := &Server{db: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		recorder := httptest.NewRecorder()
		server.getWorkspace(recorder, actualScopeWorkspaceRequest(http.MethodGet, workspace, auth.Actor{
			OrgID:         dbtest.DefaultOrgID,
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			Role:          auth.RoleViewer,
			Kind:          auth.ActorKindAPIKey,
			Permissions:   []auth.Permission{auth.PermissionFilesRead},
		}, false, ""))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
		}
		if store.getWorkspaceCalls != 1 || store.getWorkspaceByOrgAndIDCalls != 0 {
			t.Fatalf("workspace lookups = scoped:%d actual:%d", store.getWorkspaceCalls, store.getWorkspaceByOrgAndIDCalls)
		}
	})

	t.Run("different environment", func(t *testing.T) {
		store := &fakeStore{workspace: workspace}
		server := &Server{db: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		recorder := httptest.NewRecorder()
		server.getWorkspace(recorder, actualScopeWorkspaceRequest(http.MethodGet, workspace, auth.Actor{
			OrgID:         dbtest.DefaultOrgID,
			ProjectID:     projectID,
			EnvironmentID: "00000000-0000-0000-0000-000000000777",
			Role:          auth.RoleViewer,
			Kind:          auth.ActorKindAPIKey,
			Permissions:   []auth.Permission{auth.PermissionFilesRead},
		}, false, ""))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
		}
		if store.getWorkspaceCalls != 1 || store.getWorkspaceByOrgAndIDCalls != 0 {
			t.Fatalf("workspace lookups = scoped:%d actual:%d", store.getWorkspaceCalls, store.getWorkspaceByOrgAndIDCalls)
		}
	})
}

func TestScopedSessionWorkspaceLookupRetainsPathScope(t *testing.T) {
	workspace := actualScopeTestWorkspace()
	store := &fakeStore{workspace: workspace}
	server := &Server{db: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	recorder := httptest.NewRecorder()
	server.getWorkspace(recorder, actualScopeWorkspaceRequest(http.MethodGet, workspace, auth.Actor{
		OrgID: dbtest.DefaultOrgID,
		Role:  auth.RoleViewer,
		Kind:  auth.ActorKindSession,
	}, true, ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if store.getWorkspaceCalls != 1 || store.getWorkspaceByOrgAndIDCalls != 0 {
		t.Fatalf("workspace lookups = scoped:%d actual:%d", store.getWorkspaceCalls, store.getWorkspaceByOrgAndIDCalls)
	}
}

func actualScopeWorkspaceRequest(method string, workspace db.Workspace, actor auth.Actor, scoped bool, workspaceID string) *http.Request {
	request := httptest.NewRequest(method, "/api/workspaces/"+workspaceID, strings.NewReader(`{}`))
	routeContext := chi.NewRouteContext()
	if scoped {
		routeContext.URLParams.Add("projectID", pgvalue.MustUUIDValue(workspace.ProjectID).String())
		routeContext.URLParams.Add("environmentID", pgvalue.MustUUIDValue(workspace.EnvironmentID).String())
	}
	if workspaceID == "" {
		workspaceID = pgvalue.MustUUIDValue(workspace.ID).String()
	}
	routeContext.URLParams.Add("workspaceID", workspaceID)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)
	ctx = context.WithValue(ctx, actorContextKey{}, actor)
	return request.WithContext(ctx)
}

func actualScopeTestWorkspace() db.Workspace {
	return db.Workspace{
		ID:                  pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000308")),
		OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:           testProjectID(),
		EnvironmentID:       testEnvironmentID(),
		DeploymentSandboxID: pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000703")),
		SandboxID:           "default",
		SandboxFingerprint:  "sandbox-fingerprint",
		State:               db.WorkspaceStateActive,
		DesiredState:        db.WorkspaceDesiredStateActive,
		DirtyState:          db.WorkspaceDirtyStateClean,
		Metadata:            []byte(`{}`),
		CreatedAt:           testTime(),
		UpdatedAt:           testTime(),
		LastActivityAt:      testTime(),
	}
}
