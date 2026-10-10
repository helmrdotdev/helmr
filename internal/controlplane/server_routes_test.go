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
DELETE /api/projects/{projectID}/environments/{environmentID}/agents/{agentName}/slack/{publicationID}
DELETE /api/projects/{projectID}/environments/{environmentID}/api-keys/{id}
DELETE /api/projects/{projectID}/environments/{environmentID}/computers/{computerID}
DELETE /api/slack/user-links/{teamID}/{slackUserID}
DELETE /v1/computers/{computerID}
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
GET /api/projects/{projectID}/environments/{environmentID}
GET /api/projects/{projectID}/environments/{environmentID}/agents
GET /api/projects/{projectID}/environments/{environmentID}/agents/{agentName}
GET /api/projects/{projectID}/environments/{environmentID}/agents/{agentName}/slack
GET /api/projects/{projectID}/environments/{environmentID}/api-keys
GET /api/projects/{projectID}/environments/{environmentID}/commands/{commandID}
GET /api/projects/{projectID}/environments/{environmentID}/commands/{commandID}/logs
GET /api/projects/{projectID}/environments/{environmentID}/computer-definitions
GET /api/projects/{projectID}/environments/{environmentID}/computer-definitions/{computerDefinitionID}
GET /api/projects/{projectID}/environments/{environmentID}/computers
GET /api/projects/{projectID}/environments/{environmentID}/computers/{computerID}
GET /api/projects/{projectID}/environments/{environmentID}/computers/{computerID}/members
GET /api/projects/{projectID}/environments/{environmentID}/deployments
GET /api/projects/{projectID}/environments/{environmentID}/deployments/current
GET /api/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}
GET /api/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}/events
GET /api/projects/{projectID}/environments/{environmentID}/schedules
GET /api/projects/{projectID}/environments/{environmentID}/schedules/{scheduleID}
GET /api/projects/{projectID}/environments/{environmentID}/secrets
GET /api/projects/{projectID}/environments/{environmentID}/secrets/{secretID}
GET /api/projects/{projectID}/environments/{environmentID}/sessions
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/events
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/slack-delivery
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/turns
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/turns/{turnID}
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/turns/{turnID}/asks
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/turns/{turnID}/asks/{askID}
GET /api/projects/{projectRef}
GET /api/regions
GET /api/slack/link-workspaces
GET /api/slack/user-links
GET /api/slack/user-links/callback
GET /api/slack/user-links/first-use
GET /capacity/v1/deployment
GET /capacity/v1/worker-groups/resolve
GET /capacity/v1/worker-groups/{workerGroupID}/pools/resolve
GET /capacity/v1/worker-hosts
GET /capacity/v1/worker-hosts/{workerHostID}
GET /healthz
GET /readyz
GET /v1/agents
GET /v1/agents/{agentName}
GET /v1/commands/{commandID}
GET /v1/commands/{commandID}/logs
GET /v1/computer-definitions
GET /v1/computer-definitions/{computerDefinitionID}
GET /v1/computers
GET /v1/computers/{computerID}
GET /v1/computers/{computerID}/members
GET /v1/deployments
GET /v1/deployments/current
GET /v1/deployments/{deploymentID}
GET /v1/deployments/{deploymentID}/events
GET /v1/schedules
GET /v1/schedules/{scheduleID}
GET /v1/secrets
GET /v1/secrets/{secretID}
GET /v1/sessions
GET /v1/sessions/{sessionID}
GET /v1/sessions/{sessionID}/events
GET /v1/sessions/{sessionID}/turns
GET /v1/sessions/{sessionID}/turns/{turnID}
GET /v1/sessions/{sessionID}/turns/{turnID}/asks
GET /v1/sessions/{sessionID}/turns/{turnID}/asks/{askID}
GET /worker/v1/instance
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
POST /api/projects/{projectID}/environments/{environmentID}/agents/{agentName}/slack
POST /api/projects/{projectID}/environments/{environmentID}/agents/{agentName}/slack/{publicationID}/authorize
POST /api/projects/{projectID}/environments/{environmentID}/agents/{agentName}/start
POST /api/projects/{projectID}/environments/{environmentID}/api-keys
POST /api/projects/{projectID}/environments/{environmentID}/commands/{commandID}/cancel
POST /api/projects/{projectID}/environments/{environmentID}/computer-definitions/{computerDefinitionID}/computers
POST /api/projects/{projectID}/environments/{environmentID}/computers/{computerID}/exec
POST /api/projects/{projectID}/environments/{environmentID}/deployment-bundles/finalize
POST /api/projects/{projectID}/environments/{environmentID}/deployment-bundles/upload-plan
POST /api/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}/promote
POST /api/projects/{projectID}/environments/{environmentID}/secrets
POST /api/projects/{projectID}/environments/{environmentID}/secrets/{secretID}/revoke
POST /api/projects/{projectID}/environments/{environmentID}/secrets/{secretID}/rotate
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/cancel
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/close
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/enqueue
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/interrupt
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/resume
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/send
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/slack-delivery/{postID}/{recovery:check|abandon}
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/turns/{turnID}/asks/{askID}/respond
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/turns/{turnID}/messages
POST /api/slack/installations/finish
POST /api/slack/user-links/authorize
POST /api/slack/user-links/callback
POST /api/slack/user-links/confirm
POST /api/slack/user-links/verify
POST /capacity/v1/worker-groups/{workerGroupID}/plan
POST /capacity/v1/worker-hosts/{workerHostID}/drain
POST /capacity/v1/worker-hosts/{workerHostID}/lost
POST /integrations/slack/apps/{registrationID}/events
POST /integrations/slack/apps/{registrationID}/interactions
POST /v1/agents/{agentName}/start
POST /v1/commands/{commandID}/cancel
POST /v1/computer-definitions/{computerDefinitionID}/computers
POST /v1/computers/{computerID}/exec
POST /v1/deployment-bundles/finalize
POST /v1/deployment-bundles/upload-plan
POST /v1/deployments/{deploymentID}/promote
POST /v1/secrets
POST /v1/secrets/{secretID}/revoke
POST /v1/secrets/{secretID}/rotate
POST /v1/sessions/{sessionID}/cancel
POST /v1/sessions/{sessionID}/close
POST /v1/sessions/{sessionID}/enqueue
POST /v1/sessions/{sessionID}/interrupt
POST /v1/sessions/{sessionID}/resume
POST /v1/sessions/{sessionID}/send
POST /v1/sessions/{sessionID}/turns/{turnID}/asks/{askID}/respond
POST /v1/sessions/{sessionID}/turns/{turnID}/messages
POST /worker/v1/agent-computers/capture/begin
POST /worker/v1/agent-computers/capture/cancel
POST /worker/v1/agent-computers/capture/save-absence
POST /worker/v1/agent-computers/capture/seal
POST /worker/v1/agent-computers/checkpoint/complete
POST /worker/v1/agent-computers/checkpoint/read
POST /worker/v1/agent-computers/checkpoint/register
POST /worker/v1/agent-computers/controls
POST /worker/v1/agent-computers/lease/renew
POST /worker/v1/agent-computers/restore/commit
POST /worker/v1/agent-computers/restore/complete
POST /worker/v1/agent-computers/restore/prepare
POST /worker/v1/agent-computers/restore/validate
POST /worker/v1/agent-computers/source-abort/commit
POST /worker/v1/agent-computers/source-abort/complete
POST /worker/v1/agent-computers/source-abort/prepare
POST /worker/v1/agent-computers/source-abort/validate
POST /worker/v1/agent-computers/stopped
POST /worker/v1/allocations/computer/deliver
POST /worker/v1/allocations/computer/processes
POST /worker/v1/allocations/computer/ready
POST /worker/v1/allocations/computer/source
POST /worker/v1/allocations/list
POST /worker/v1/allocations/preparation/capture
POST /worker/v1/allocations/preparation/capture/begin
POST /worker/v1/allocations/preparation/deliver
POST /worker/v1/allocations/preparation/fail
POST /worker/v1/allocations/preparation/key
POST /worker/v1/allocations/preparation/logs
POST /worker/v1/allocations/preparation/objects/certify
POST /worker/v1/allocations/preparation/objects/register
POST /worker/v1/allocations/preparation/publish
POST /worker/v1/allocations/preparation/renew
POST /worker/v1/allocations/preparation/secrets
POST /worker/v1/allocations/preparation/start
POST /worker/v1/allocations/preparation/stopped
POST /worker/v1/computer-commands/claim
POST /worker/v1/computer-commands/complete
POST /worker/v1/computer-commands/logs/append
POST /worker/v1/computer-commands/reconcile
POST /worker/v1/computer-saves/capture
POST /worker/v1/computer-saves/next
POST /worker/v1/computer-saves/objects/certify
POST /worker/v1/computer-saves/objects/register
POST /worker/v1/computer-saves/publish
POST /worker/v1/enrollment
POST /worker/v1/instance/activate
POST /worker/v1/instance/credential
POST /worker/v1/instance/drain
POST /worker/v1/instance/drain/complete
POST /worker/v1/instance/fence
POST /worker/v1/instance/observations
POST /worker/v1/instance/recover
POST /worker/v1/run/secret-proxy/prepare
POST /worker/v1/run/secret-proxy/resolve
POST /worker/v1/sessions/attachment
POST /worker/v1/sessions/authority
POST /worker/v1/sessions/control
POST /worker/v1/sessions/control/receipt
POST /worker/v1/sessions/failed
POST /worker/v1/sessions/logs
POST /worker/v1/sessions/message
POST /worker/v1/sessions/message/receipt
POST /worker/v1/sessions/operations
POST /worker/v1/sessions/ready
POST /worker/v1/sessions/start
POST /worker/v1/sessions/start/release
POST /worker/v1/sessions/stopped
POST /worker/v1/sessions/turn
POST /worker/v1/sessions/turn/receipt
PUT /api/projects/{projectID}/environments/{environmentID}/agents/{agentName}/slack/{publicationID}/credentials
PUT /capacity/v1/worker-groups/{workerGroupID}/primary-pool
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
		{name: "old Capacity root", method: http.MethodGet, path: "/api/capacity/v0/worker-hosts", status: http.StatusNotFound, code: "not_found"},
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
	cfg := completeServerConfig(t)
	cfg.CapacityToken = capacityTestToken()
	router, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name          string
		path          string
		authorization string
		status        int
	}{
		{name: "Capacity missing", path: "/capacity/v1/worker-hosts", status: http.StatusUnauthorized},
		{name: "Capacity foreign", path: "/capacity/v1/worker-hosts", authorization: "Bearer hlmr_test_product", status: http.StatusUnauthorized},
		{name: "Save missing", path: "/worker/v1/computer-saves/next", status: http.StatusUnauthorized},
		{name: "Save foreign", path: "/worker/v1/computer-saves/publish", authorization: "Bearer " + capacityTestToken(), status: http.StatusUnauthorized},
		{name: "Worker missing", path: "/worker/v1/instance", status: http.StatusUnauthorized},
		{name: "Worker foreign", path: "/worker/v1/instance", authorization: "Bearer " + capacityTestToken(), status: http.StatusUnauthorized},
		{name: "Worker enrollment bootstrap", path: "/worker/v1/enrollment", status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, nil)
			if test.path == "/worker/v1/instance" || test.path == "/capacity/v1/worker-hosts" {
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

func TestWorkerRouteRejectsMalformedJWTGroupBeforeDatabase(t *testing.T) {
	// completeServerConfig has no database: a query would fail the request.
	router, err := NewServer(completeServerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	claims := rawWorkerJWTClaims("01900000-0000-7000-8000-000000000711", "not-a-uuid", "01900000-0000-7000-8000-000000000712")
	request := httptest.NewRequest(http.MethodGet, "/worker/v1/instance", nil)
	request.Header.Set("Authorization", "Bearer "+signRawWorkerJWT(t, claims))
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", response.Code, response.Body.String())
	}
}

// Capacity route limits are covered through NewServer in
// TestCapacityHTTPPreservesRequestBodyLimits.
func TestWorkerRoutesPreserveRequestBodyLimits(t *testing.T) {
	router, err := NewServer(completeServerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/instance/observations", strings.NewReader("x"))
	request.ContentLength = apiRequestBodyLimit + 1
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusRequestEntityTooLarge, response.Body.String())
	}
	if got := decodeHTTPError(t, response.Body.Bytes()).Code; got != "request_too_large" {
		t.Fatalf("code = %q, want request_too_large", got)
	}
}

func TestProcessDiagnosticsRemainInternal(t *testing.T) {
	server := &Server{log: discardTestLogger()}
	router := chi.NewRouter()
	server.mountRoutes(router)
	router.NotFound(server.notFound)
	registered := map[string]bool{}
	if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"/v1", "/api/projects/{projectID}/environments/{environmentID}"} {
		for _, owner := range []string{"/sessions/{sessionID}", "/computer-preparations/{preparationID}"} {
			for _, suffix := range []string{"/logs", "/log-streams"} {
				path := prefix + owner + suffix
				if registered["GET "+path] {
					t.Fatalf("process diagnostics exposed: %s", path)
				}
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
				if recorder.Code != http.StatusNotFound {
					t.Fatalf("%s returned %d, want 404", path, recorder.Code)
				}
			}
		}
	}
	for _, route := range []string{
		"POST /worker/v1/sessions/logs", "POST /worker/v1/allocations/preparation/logs",
		"GET /v1/commands/{commandID}/logs",
	} {
		if !registered[route] {
			t.Fatalf("required diagnostic ingestion or command output removed: %s", route)
		}
	}
}
