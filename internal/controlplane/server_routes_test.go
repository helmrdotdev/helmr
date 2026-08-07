package controlplane

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func TestControlPlaneRoutesMatchCurrentProtocol(t *testing.T) {
	router := chi.NewRouter()
	server := &Server{}
	server.mountRoutes(router)
	var routes chi.Routes = router

	var got []string
	if err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got = append(got, method+" "+route)
		return nil
	}); err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	sort.Strings(got)

	want := strings.Split(strings.TrimSpace(`
DELETE /api/invitations/{id}
DELETE /api/members/{userID}
DELETE /api/projects/{projectID}
DELETE /api/projects/{projectID}/environments/{environmentID}
DELETE /api/projects/{projectID}/environments/{environmentID}/api-keys/{id}
DELETE /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}
DELETE /v1/workspaces/{workspaceID}
GET /admin/api/v1/regions
GET /admin/api/v1/regions/{regionID}
GET /admin/api/v1/worker-groups
GET /admin/api/v1/worker-groups/{groupID}
GET /api/auth/device/status
GET /api/capacity/v0/worker-groups/resolve
GET /api/capacity/v0/worker-instances
GET /api/capacity/v0/worker-instances/{workerInstanceID}
GET /api/invitations
GET /api/me
GET /api/members
GET /api/projects
GET /api/projects/{projectID}
GET /api/projects/{projectID}/environments/{environmentID}
GET /api/projects/{projectID}/environments/{environmentID}/actors
GET /api/projects/{projectID}/environments/{environmentID}/actors/{actorID}
GET /api/projects/{projectID}/environments/{environmentID}/api-keys
GET /api/projects/{projectID}/environments/{environmentID}/deployments
GET /api/projects/{projectID}/environments/{environmentID}/deployments/current
GET /api/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}
GET /api/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}/events
GET /api/projects/{projectID}/environments/{environmentID}/runs
GET /api/projects/{projectID}/environments/{environmentID}/runs/{runID}
GET /api/projects/{projectID}/environments/{environmentID}/runs/{runID}/events
GET /api/projects/{projectID}/environments/{environmentID}/runs/{runID}/logs
GET /api/projects/{projectID}/environments/{environmentID}/sandboxes
GET /api/projects/{projectID}/environments/{environmentID}/sandboxes/{sandboxID}
GET /api/projects/{projectID}/environments/{environmentID}/schedules
GET /api/projects/{projectID}/environments/{environmentID}/schedules/{scheduleID}
GET /api/projects/{projectID}/environments/{environmentID}/secrets
GET /api/projects/{projectID}/environments/{environmentID}/secrets/{secretID}
GET /api/projects/{projectID}/environments/{environmentID}/sessions
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/outputs
GET /api/projects/{projectID}/environments/{environmentID}/tasks
GET /api/projects/{projectID}/environments/{environmentID}/tasks/{taskID}
GET /api/projects/{projectID}/environments/{environmentID}/tokens
GET /api/projects/{projectID}/environments/{environmentID}/tokens/{tokenID}
GET /api/projects/{projectID}/environments/{environmentID}/workspaces
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/files
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/files/content
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/files/stat
GET /api/regions
GET /api/worker/v0/instance
GET /healthz
GET /readyz
GET /v1/actors
GET /v1/actors/{actorID}
GET /v1/deployments
GET /v1/deployments/current
GET /v1/deployments/{deploymentID}
GET /v1/deployments/{deploymentID}/events
GET /v1/runs
GET /v1/runs/{runID}
GET /v1/runs/{runID}/events
GET /v1/runs/{runID}/logs
GET /v1/sandboxes
GET /v1/sandboxes/{sandboxID}
GET /v1/schedules
GET /v1/schedules/{scheduleID}
GET /v1/secrets
GET /v1/secrets/{secretID}
GET /v1/sessions
GET /v1/sessions/{sessionID}
GET /v1/sessions/{sessionID}/outputs
GET /v1/tasks
GET /v1/tasks/{taskID}
GET /v1/tokens
GET /v1/tokens/{tokenID}
GET /v1/workspaces
GET /v1/workspaces/{workspaceID}
GET /v1/workspaces/{workspaceID}/files
GET /v1/workspaces/{workspaceID}/files/content
GET /v1/workspaces/{workspaceID}/files/stat
OPTIONS /api/public/tokens/{tokenID}/complete
PATCH /admin/api/v1/regions/{regionID}
PATCH /admin/api/v1/worker-groups/{groupID}
PATCH /api/members/{userID}
PATCH /api/projects/{projectID}
PATCH /api/projects/{projectID}/environments/{environmentID}
POST /admin/api/v1/regions
POST /admin/api/v1/worker-groups
POST /admin/api/v1/worker-groups/{groupID}/activate
POST /admin/api/v1/worker-groups/{groupID}/disable
POST /admin/api/v1/worker-groups/{groupID}/drain
POST /admin/api/v1/worker-groups/{groupID}/pause
POST /admin/api/v1/worker-groups/{groupID}/token/rotate
POST /api/auth/device/approve
POST /api/auth/device/deny
POST /api/auth/device/start
POST /api/auth/device/token
POST /api/auth/github/finish
POST /api/auth/github/invite/start
POST /api/auth/github/start
POST /api/auth/logout
POST /api/auth/magic-link/finish
POST /api/auth/magic-link/invite/start
POST /api/auth/magic-link/start
POST /api/capacity/v0/worker-groups/{workerGroupID}/plan
POST /api/capacity/v0/worker-instances/{workerInstanceID}/drain
POST /api/invitations
POST /api/organizations
POST /api/projects
POST /api/projects/{projectID}/environments
POST /api/projects/{projectID}/environments/{environmentID}/actors/{actorDeclaredID}/start
POST /api/projects/{projectID}/environments/{environmentID}/api-keys
POST /api/projects/{projectID}/environments/{environmentID}/deployments
POST /api/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}/promote
POST /api/projects/{projectID}/environments/{environmentID}/runs/{runID}/cancel
POST /api/projects/{projectID}/environments/{environmentID}/sandboxes/{sandboxID}/workspaces
POST /api/projects/{projectID}/environments/{environmentID}/secrets
POST /api/projects/{projectID}/environments/{environmentID}/secrets/{secretID}/revoke
POST /api/projects/{projectID}/environments/{environmentID}/secrets/{secretID}/rotate
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/close
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/inputs
POST /api/projects/{projectID}/environments/{environmentID}/tasks/{taskDeclaredID}/start
POST /api/projects/{projectID}/environments/{environmentID}/tokens
POST /api/projects/{projectID}/environments/{environmentID}/tokens/{tokenID}/cancel
POST /api/projects/{projectID}/environments/{environmentID}/tokens/{tokenID}/complete
POST /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/exec
POST /api/public/tokens/{tokenID}/complete
POST /api/token-callbacks/{tokenID}/{callbackSecret}
POST /api/worker/v0/build/deployments/complete
POST /api/worker/v0/build/deployments/delivery-failed
POST /api/worker/v0/build/deployments/lease
POST /api/worker/v0/build/deployments/reject
POST /api/worker/v0/build/deployments/renew
POST /api/worker/v0/build/deployments/start
POST /api/worker/v0/build/deployments/workspace-images/admit
POST /api/worker/v0/build/deployments/workspace-images/complete
POST /api/worker/v0/build/deployments/workspace-images/credentials
POST /api/worker/v0/build/platform-acquisitions/complete
POST /api/worker/v0/build/platform-acquisitions/fail
POST /api/worker/v0/build/platform-acquisitions/next
POST /api/worker/v0/enrollment
POST /api/worker/v0/instance/activate
POST /api/worker/v0/instance/drain
POST /api/worker/v0/instance/drain/complete
POST /api/worker/v0/instance/fence
POST /api/worker/v0/instance/observations
POST /api/worker/v0/instance/recover
POST /api/worker/v0/instance/token
POST /api/worker/v0/run/actors/start
POST /api/worker/v0/run/checkpoints/failed
POST /api/worker/v0/run/checkpoints/ready
POST /api/worker/v0/run/finalization/begin
POST /api/worker/v0/run/leases/claim
POST /api/worker/v0/run/leases/discover
POST /api/worker/v0/run/leases/entrypoint
POST /api/worker/v0/run/leases/renew
POST /api/worker/v0/run/leases/resume-release
POST /api/worker/v0/run/leases/start
POST /api/worker/v0/run/logs/append
POST /api/worker/v0/run/metadata/update
POST /api/worker/v0/run/runtime-instances/closed
POST /api/worker/v0/run/runtime-instances/failed
POST /api/worker/v0/run/runtime-instances/ready
POST /api/worker/v0/run/runtime-instances/reconcile
POST /api/worker/v0/run/runtime-substrates/register
POST /api/worker/v0/run/sessions/close
POST /api/worker/v0/run/sessions/complete
POST /api/worker/v0/run/sessions/inputs/send
POST /api/worker/v0/run/sessions/outputs/append
POST /api/worker/v0/run/sessions/outputs/read-page
POST /api/worker/v0/run/sessions/retrieve
POST /api/worker/v0/run/sessions/turns/commit
POST /api/worker/v0/run/structured-logs/append
POST /api/worker/v0/run/tasks/complete
POST /api/worker/v0/run/tasks/invoke
POST /api/worker/v0/run/tokens/create
POST /api/worker/v0/run/waits/create
POST /api/worker/v0/run/waits/poll
POST /api/worker/v0/run/waits/resume-ack
POST /api/worker/v0/run/workspace-execs/claim
POST /api/worker/v0/run/workspace-execs/complete
POST /api/worker/v0/run/workspace-mounts/capture
POST /api/worker/v0/run/workspace-mounts/claim
POST /api/worker/v0/run/workspace-mounts/fail
POST /api/worker/v0/run/workspace-mounts/mounted
POST /api/worker/v0/run/workspace-mounts/renew
POST /api/worker/v0/run/workspace-mounts/stop
POST /api/worker/v0/run/workspaces/create
POST /api/worker/v0/run/workspaces/delete
POST /api/worker/v0/run/workspaces/exec
POST /api/worker/v0/run/workspaces/exec/poll
POST /api/worker/v0/run/workspaces/files/list
POST /api/worker/v0/run/workspaces/files/read
POST /api/worker/v0/run/workspaces/files/stat
POST /api/worker/v0/run/workspaces/retrieve
POST /v1/actors/{actorDeclaredID}/start
POST /v1/deployments
POST /v1/deployments/{deploymentID}/promote
POST /v1/runs/{runID}/cancel
POST /v1/sandboxes/{sandboxID}/workspaces
POST /v1/secrets
POST /v1/secrets/{secretID}/revoke
POST /v1/secrets/{secretID}/rotate
POST /v1/sessions/{sessionID}/close
POST /v1/sessions/{sessionID}/inputs
POST /v1/tasks/{taskDeclaredID}/start
POST /v1/tokens
POST /v1/tokens/{tokenID}/cancel
POST /v1/tokens/{tokenID}/complete
POST /v1/workspaces/{workspaceID}/exec
`), "\n")
	if !slices.IsSorted(want) {
		t.Fatal("Control Plane route snapshot must stay sorted")
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Control Plane routes changed\nwant:\n%s\n\ngot:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}

func TestRouterFallbacksUseHTTPErrorEnvelope(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	router := chi.NewRouter()
	router.Use(server.requestCorrelation)
	server.mountRoutes(router)
	router.NotFound(server.notFound)
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		server.methodNotAllowed(router, w, r)
	})

	for _, test := range []struct {
		name   string
		method string
		path   string
		status int
		code   string
	}{
		{name: "not found", method: http.MethodGet, path: "/v1/missing", status: http.StatusNotFound, code: "not_found"},
		{name: "method not allowed", method: http.MethodPost, path: "/healthz", status: http.StatusMethodNotAllowed, code: "method_not_allowed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			if got := decodeHTTPError(t, response.Body.Bytes()).Code; got != test.code {
				t.Fatalf("code = %q, want %q", got, test.code)
			}
			if _, err := uuid.Parse(response.Header().Get(requestIDHeader)); err != nil {
				t.Fatalf("%s is not a UUID: %v", requestIDHeader, err)
			}
			if test.status == http.StatusMethodNotAllowed && response.Header().Get("Allow") != http.MethodGet {
				t.Fatalf("Allow = %q, want %q", response.Header().Get("Allow"), http.MethodGet)
			}
		})
	}
}
