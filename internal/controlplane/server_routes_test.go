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

	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/db"
)

type routeWorkerAuthStore struct{ db.Querier }

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
GET /admin/api/v1/worker-groups/{groupID}/pools
GET /api/auth/device/status
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
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/exec/{processID}
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/files
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/files/content
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/files/stat
GET /api/regions
GET /capacity/v1/worker-groups/resolve
GET /capacity/v1/worker-groups/{workerGroupID}/pools/resolve
GET /capacity/v1/worker-instances
GET /capacity/v1/worker-instances/{workerInstanceID}
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
GET /v1/workspaces/{workspaceID}/exec/{processID}
GET /v1/workspaces/{workspaceID}/files
GET /v1/workspaces/{workspaceID}/files/content
GET /v1/workspaces/{workspaceID}/files/stat
GET /worker/v1/instance
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
POST /admin/api/v1/worker-groups/{groupID}/pools
POST /admin/api/v1/worker-groups/{groupID}/pools/{poolID}/disable
POST /admin/api/v1/worker-groups/{groupID}/pools/{poolID}/drain
POST /admin/api/v1/worker-groups/{groupID}/pools/{poolID}/primary
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
POST /api/invitations
POST /api/organizations
POST /api/projects
POST /api/projects/{projectID}/environments
POST /api/projects/{projectID}/environments/{environmentID}/actors/{actorDeclaredID}/start
POST /api/projects/{projectID}/environments/{environmentID}/api-keys
POST /api/projects/{projectID}/environments/{environmentID}/deployment-bundles/finalize
POST /api/projects/{projectID}/environments/{environmentID}/deployment-bundles/upload-plan
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
POST /capacity/v1/worker-groups/{workerGroupID}/plan
POST /capacity/v1/worker-instances/{workerInstanceID}/drain
POST /capacity/v1/worker-instances/{workerInstanceID}/lost
POST /v1/actors/{actorDeclaredID}/start
POST /v1/deployment-bundles/finalize
POST /v1/deployment-bundles/upload-plan
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
POST /worker/v1/enrollment
POST /worker/v1/instance/activate
POST /worker/v1/instance/drain
POST /worker/v1/instance/drain/complete
POST /worker/v1/instance/fence
POST /worker/v1/instance/observations
POST /worker/v1/instance/recover
POST /worker/v1/instance/token
POST /worker/v1/run/actors/start
POST /worker/v1/run/checkpoints/failed
POST /worker/v1/run/checkpoints/ready
POST /worker/v1/run/finalization/begin
POST /worker/v1/run/leases/claim
POST /worker/v1/run/leases/discover
POST /worker/v1/run/leases/entrypoint
POST /worker/v1/run/leases/renew
POST /worker/v1/run/leases/resume-release
POST /worker/v1/run/leases/start
POST /worker/v1/run/logs/append
POST /worker/v1/run/metadata/update
POST /worker/v1/run/runtime-instances/closed
POST /worker/v1/run/runtime-instances/failed
POST /worker/v1/run/runtime-instances/ready
POST /worker/v1/run/runtime-instances/reconcile
POST /worker/v1/run/runtime-substrates/register
POST /worker/v1/run/sessions/close
POST /worker/v1/run/sessions/complete
POST /worker/v1/run/sessions/inputs/send
POST /worker/v1/run/sessions/outputs/append
POST /worker/v1/run/sessions/outputs/read-page
POST /worker/v1/run/sessions/retrieve
POST /worker/v1/run/sessions/turns/commit
POST /worker/v1/run/structured-logs/append
POST /worker/v1/run/tasks/complete
POST /worker/v1/run/tasks/invoke
POST /worker/v1/run/tokens/create
POST /worker/v1/run/waits/create
POST /worker/v1/run/waits/poll
POST /worker/v1/run/waits/resume-ack
POST /worker/v1/run/workspace-execs/claim
POST /worker/v1/run/workspace-execs/complete
POST /worker/v1/run/workspace-mounts/capture
POST /worker/v1/run/workspace-mounts/claim
POST /worker/v1/run/workspace-mounts/fail
POST /worker/v1/run/workspace-mounts/mounted
POST /worker/v1/run/workspace-mounts/renew
POST /worker/v1/run/workspace-mounts/stop
POST /worker/v1/run/workspaces/create
POST /worker/v1/run/workspaces/delete
POST /worker/v1/run/workspaces/exec
POST /worker/v1/run/workspaces/exec/poll
POST /worker/v1/run/workspaces/files/list
POST /worker/v1/run/workspaces/files/read
POST /worker/v1/run/workspaces/files/stat
POST /worker/v1/run/workspaces/retrieve
PUT /capacity/v1/worker-groups/{workerGroupID}/primary-pools
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
		{name: "Developer API not found", method: http.MethodGet, path: "/v1/missing", status: http.StatusNotFound, code: "not_found"},
		{name: "Capacity not found", method: http.MethodGet, path: "/capacity/v1/missing", status: http.StatusNotFound, code: "not_found"},
		{name: "Worker not found", method: http.MethodGet, path: "/worker/v1/missing", status: http.StatusNotFound, code: "not_found"},
		{name: "old Capacity root", method: http.MethodGet, path: "/api/capacity/v0/worker-instances", status: http.StatusNotFound, code: "not_found"},
		{name: "old Worker root", method: http.MethodGet, path: "/api/worker/v0/instance", status: http.StatusNotFound, code: "not_found"},
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
			if location := response.Header().Get("Location"); location != "" {
				t.Fatalf("Location = %q, want no redirect", location)
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

func TestMachineRoutesPreserveAuthenticationBoundaries(t *testing.T) {
	capacityTokenHash, err := hashCapacityToken(capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		db:                    &routeWorkerAuthStore{},
		capacityTokenHash:     capacityTokenHash,
		workerTokenSigningKey: []byte(strings.Repeat("w", 32)),
		workerEnrollmentGuard: newWorkerEnrollmentGuard(),
	}
	router := chi.NewRouter()
	server.mountRoutes(router)

	for _, test := range []struct {
		name          string
		path          string
		authorization string
		status        int
	}{
		{name: "Capacity missing", path: "/capacity/v1/worker-instances", status: http.StatusUnauthorized},
		{name: "Capacity foreign", path: "/capacity/v1/worker-instances", authorization: "Bearer hlmr_test_product", status: http.StatusUnauthorized},
		{name: "Worker missing", path: "/worker/v1/instance", status: http.StatusUnauthorized},
		{name: "Worker foreign", path: "/worker/v1/instance", authorization: "Bearer " + capacityTestToken(), status: http.StatusUnauthorized},
		{name: "Worker enrollment bootstrap", path: "/worker/v1/enrollment", status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, nil)
			if test.path == "/worker/v1/instance" || test.path == "/capacity/v1/worker-instances" {
				request.Method = http.MethodGet
			}
			request.Header.Set("Authorization", test.authorization)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
}

func TestMachineRoutesPreserveRequestBodyLimits(t *testing.T) {
	capacityTokenHash, err := hashCapacityToken(capacityTestToken())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{capacityTokenHash: capacityTokenHash}
	router := chi.NewRouter()
	server.mountRoutes(router)

	for _, test := range []struct {
		name   string
		path   string
		length int64
	}{
		{name: "Capacity common limit", path: "/capacity/v1/worker-instances/01900000-0000-7000-8000-000000000000/lost", length: apiRequestBodyLimit + 1},
		{name: "Worker common limit", path: "/worker/v1/instance/observations", length: apiRequestBodyLimit + 1},
		{name: "Capacity mutation limit", path: "/capacity/v1/worker-groups/01900000-0000-7000-8000-000000000000/plan", length: capacityRequestBodyLimit + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader("x"))
			request.ContentLength = test.length
			request.Header.Set("Authorization", "Bearer "+capacityTestToken())
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusRequestEntityTooLarge, response.Body.String())
			}
			if got := decodeHTTPError(t, response.Body.Bytes()).Code; got != "request_too_large" {
				t.Fatalf("code = %q, want request_too_large", got)
			}
		})
	}
}
