package control

import (
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestControlRoutes(t *testing.T) {
	handler := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	routes, ok := handler.(chi.Routes)
	if !ok {
		t.Fatalf("control.New returned %T, want chi.Routes", handler)
	}

	var got []string
	if err := chi.Walk(routes, func(method string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
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
DELETE /api/projects/{projectID}/environments/{environmentID}/schedules/{id}
DELETE /api/projects/{projectID}/environments/{environmentID}/secrets/{name}
DELETE /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}
DELETE /api/schedules/{id}
DELETE /api/secrets/{name}
DELETE /api/workspaces/{workspaceID}
GET /api/auth/device/status
GET /api/deployments
GET /api/deployments/current
GET /api/deployments/{deploymentID}
GET /api/deployments/{deploymentID}/events
GET /api/invitations
GET /api/me
GET /api/members
GET /api/projects
GET /api/projects/{projectID}
GET /api/projects/{projectID}/environments/{environmentID}
GET /api/projects/{projectID}/environments/{environmentID}/api-keys
GET /api/projects/{projectID}/environments/{environmentID}/deployments
GET /api/projects/{projectID}/environments/{environmentID}/deployments/current
GET /api/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}
GET /api/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}/events
GET /api/projects/{projectID}/environments/{environmentID}/runs
GET /api/projects/{projectID}/environments/{environmentID}/runs/counts
GET /api/projects/{projectID}/environments/{environmentID}/runs/{id}
GET /api/projects/{projectID}/environments/{environmentID}/runs/{id}/events
GET /api/projects/{projectID}/environments/{environmentID}/runs/{id}/logs
GET /api/projects/{projectID}/environments/{environmentID}/sandboxes
GET /api/projects/{projectID}/environments/{environmentID}/sandboxes/{sandboxID}
GET /api/projects/{projectID}/environments/{environmentID}/schedules
GET /api/projects/{projectID}/environments/{environmentID}/schedules/{id}
GET /api/projects/{projectID}/environments/{environmentID}/secrets
GET /api/projects/{projectID}/environments/{environmentID}/secrets/{name}
GET /api/projects/{projectID}/environments/{environmentID}/sessions
GET /api/projects/{projectID}/environments/{environmentID}/sessions/by-external-id
GET /api/projects/{projectID}/environments/{environmentID}/sessions/by-external-id/inputs/{stream}
GET /api/projects/{projectID}/environments/{environmentID}/sessions/by-external-id/outputs/{stream}
GET /api/projects/{projectID}/environments/{environmentID}/sessions/by-external-id/outputs/{stream}/read
GET /api/projects/{projectID}/environments/{environmentID}/sessions/by-external-id/runs
GET /api/projects/{projectID}/environments/{environmentID}/sessions/by-external-id/streams
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/inputs/{stream}
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/outputs/{stream}
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/outputs/{stream}/read
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/runs
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/streams
GET /api/projects/{projectID}/environments/{environmentID}/tasks
GET /api/projects/{projectID}/environments/{environmentID}/tasks/{taskID}
GET /api/projects/{projectID}/environments/{environmentID}/tokens
GET /api/projects/{projectID}/environments/{environmentID}/tokens/{tokenID}
GET /api/projects/{projectID}/environments/{environmentID}/workspaces
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/execs
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/execs/{execID}
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/execs/{execID}/stderr
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/execs/{execID}/stdout
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/pty
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/pty/{ptyID}
GET /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/pty/{ptyID}/output
GET /api/runs
GET /api/runs/counts
GET /api/runs/{id}
GET /api/runs/{id}/events
GET /api/runs/{id}/logs
GET /api/sandboxes
GET /api/sandboxes/{sandboxID}
GET /api/schedules
GET /api/schedules/{id}
GET /api/secrets
GET /api/secrets/{name}
GET /api/sessions
GET /api/sessions/by-external-id
GET /api/sessions/by-external-id/inputs/{stream}
GET /api/sessions/by-external-id/outputs/{stream}
GET /api/sessions/by-external-id/outputs/{stream}/read
GET /api/sessions/by-external-id/runs
GET /api/sessions/by-external-id/streams
GET /api/sessions/{sessionID}
GET /api/sessions/{sessionID}/inputs/{stream}
GET /api/sessions/{sessionID}/outputs/{stream}
GET /api/sessions/{sessionID}/outputs/{stream}/read
GET /api/sessions/{sessionID}/runs
GET /api/sessions/{sessionID}/streams
GET /api/tasks
GET /api/tasks/{taskID}
GET /api/tokens
GET /api/tokens/{tokenID}
GET /api/v1/sessions/by-external-id/outputs/{stream}/read
GET /api/v1/sessions/{sessionID}/outputs/{stream}/read
GET /api/worker/status
GET /api/workspaces
GET /api/workspaces/{workspaceID}
GET /api/workspaces/{workspaceID}/execs
GET /api/workspaces/{workspaceID}/execs/{execID}
GET /api/workspaces/{workspaceID}/execs/{execID}/stderr
GET /api/workspaces/{workspaceID}/execs/{execID}/stdout
GET /api/workspaces/{workspaceID}/pty
GET /api/workspaces/{workspaceID}/pty/{ptyID}
GET /api/workspaces/{workspaceID}/pty/{ptyID}/output
GET /healthz
GET /readyz
OPTIONS /api/v1/sessions/by-external-id/inputs/{stream}
OPTIONS /api/v1/sessions/by-external-id/outputs/{stream}/read
OPTIONS /api/v1/sessions/{sessionID}/inputs/{stream}
OPTIONS /api/v1/sessions/{sessionID}/outputs/{stream}/read
OPTIONS /api/v1/tokens/{tokenID}/complete
PATCH /api/members/{userID}
PATCH /api/projects/{projectID}
PATCH /api/projects/{projectID}/environments/{environmentID}
PATCH /api/projects/{projectID}/environments/{environmentID}/sessions/by-external-id
PATCH /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}
PATCH /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}
PATCH /api/sessions/by-external-id
PATCH /api/sessions/{sessionID}
PATCH /api/workspaces/{workspaceID}
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
POST /api/deployments
POST /api/deployments/{deployment}/promote
POST /api/invitations
POST /api/organizations
POST /api/projects
POST /api/projects/{projectID}/environments
POST /api/projects/{projectID}/environments/{environmentID}/api-keys
POST /api/projects/{projectID}/environments/{environmentID}/deployments
POST /api/projects/{projectID}/environments/{environmentID}/deployments/{deployment}/promote
POST /api/projects/{projectID}/environments/{environmentID}/runs/{id}/cancel
POST /api/projects/{projectID}/environments/{environmentID}/schedules
POST /api/projects/{projectID}/environments/{environmentID}/schedules/{id}/activate
POST /api/projects/{projectID}/environments/{environmentID}/schedules/{id}/deactivate
POST /api/projects/{projectID}/environments/{environmentID}/sessions
POST /api/projects/{projectID}/environments/{environmentID}/sessions/by-external-id/cancel
POST /api/projects/{projectID}/environments/{environmentID}/sessions/by-external-id/close
POST /api/projects/{projectID}/environments/{environmentID}/sessions/by-external-id/inputs/{stream}
POST /api/projects/{projectID}/environments/{environmentID}/sessions/by-external-id/outputs/{stream}
POST /api/projects/{projectID}/environments/{environmentID}/sessions/start-and-wait
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/cancel
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/close
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/inputs/{stream}
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/outputs/{stream}
POST /api/projects/{projectID}/environments/{environmentID}/tokens
POST /api/projects/{projectID}/environments/{environmentID}/tokens/{tokenID}/cancel
POST /api/projects/{projectID}/environments/{environmentID}/tokens/{tokenID}/complete
POST /api/projects/{projectID}/environments/{environmentID}/workspaces
POST /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/connect
POST /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/execs
POST /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/execs/{execID}/stdin
POST /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/execs/{execID}/stdin/close
POST /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/materialize
POST /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/pty
POST /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/pty/{ptyID}/close
POST /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/pty/{ptyID}/input
POST /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/pty/{ptyID}/resize
POST /api/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/stop
POST /api/public-access-tokens
POST /api/runs/{id}/cancel
POST /api/schedules
POST /api/schedules/{id}/activate
POST /api/schedules/{id}/deactivate
POST /api/sessions
POST /api/sessions/by-external-id/cancel
POST /api/sessions/by-external-id/close
POST /api/sessions/by-external-id/inputs/{stream}
POST /api/sessions/by-external-id/outputs/{stream}
POST /api/sessions/start-and-wait
POST /api/sessions/{sessionID}/cancel
POST /api/sessions/{sessionID}/close
POST /api/sessions/{sessionID}/inputs/{stream}
POST /api/sessions/{sessionID}/outputs/{stream}
POST /api/tokens
POST /api/tokens/{tokenID}/cancel
POST /api/tokens/{tokenID}/complete
POST /api/v1/sessions/by-external-id/inputs/{stream}
POST /api/v1/sessions/{sessionID}/inputs/{stream}
POST /api/v1/tokens/{tokenID}/callback/{callbackSecret}
POST /api/v1/tokens/{tokenID}/complete
POST /api/worker/activate
POST /api/worker/auth/token
POST /api/worker/deployments/complete
POST /api/worker/deployments/lease
POST /api/worker/drain
POST /api/worker/leases/checkpoints/failed
POST /api/worker/leases/checkpoints/ready
POST /api/worker/leases/lease
POST /api/worker/leases/log-entries
POST /api/worker/leases/logs
POST /api/worker/leases/metadata
POST /api/worker/leases/release
POST /api/worker/leases/renew
POST /api/worker/leases/restores/ack
POST /api/worker/leases/run-waits
POST /api/worker/leases/run-waits/workspace-capture
POST /api/worker/leases/start
POST /api/worker/leases/streams/input/read
POST /api/worker/leases/streams/output
POST /api/worker/leases/tokens
POST /api/worker/register
POST /api/worker/workspaces/execs/exited
POST /api/worker/workspaces/execs/input
POST /api/worker/workspaces/execs/input-delivered
POST /api/worker/workspaces/execs/output
POST /api/worker/workspaces/execs/started
POST /api/worker/workspaces/materializations/capture
POST /api/worker/workspaces/materializations/claim
POST /api/worker/workspaces/materializations/fail
POST /api/worker/workspaces/materializations/operations/claim
POST /api/worker/workspaces/materializations/operations/complete
POST /api/worker/workspaces/materializations/operations/start
POST /api/worker/workspaces/materializations/renew
POST /api/worker/workspaces/materializations/running
POST /api/worker/workspaces/materializations/stop
POST /api/worker/workspaces/ptys/closed
POST /api/worker/workspaces/ptys/input
POST /api/worker/workspaces/ptys/input-delivered
POST /api/worker/workspaces/ptys/opened
POST /api/worker/workspaces/ptys/output
POST /api/worker/workspaces/ptys/resize-applied
POST /api/workspaces
POST /api/workspaces/{workspaceID}/connect
POST /api/workspaces/{workspaceID}/execs
POST /api/workspaces/{workspaceID}/execs/{execID}/stdin
POST /api/workspaces/{workspaceID}/execs/{execID}/stdin/close
POST /api/workspaces/{workspaceID}/materialize
POST /api/workspaces/{workspaceID}/pty
POST /api/workspaces/{workspaceID}/pty/{ptyID}/close
POST /api/workspaces/{workspaceID}/pty/{ptyID}/input
POST /api/workspaces/{workspaceID}/pty/{ptyID}/resize
POST /api/workspaces/{workspaceID}/stop
PUT /api/projects/{projectID}/environments/{environmentID}/schedules/{id}
PUT /api/projects/{projectID}/environments/{environmentID}/secrets/{name}
PUT /api/schedules/{id}
PUT /api/secrets/{name}
`), "\n")
	if !slices.IsSorted(want) {
		t.Fatal("control route snapshot must stay sorted")
	}
	if !slices.Equal(got, want) {
		missing, unexpected := routeDiff(want, got)
		t.Fatalf("control routes changed\nmissing:\n%s\n\nunexpected:\n%s", joinRoutes(missing), joinRoutes(unexpected))
	}
}

func routeDiff(want, got []string) (missing []string, unexpected []string) {
	gotSet := make(map[string]struct{}, len(got))
	for _, route := range got {
		gotSet[route] = struct{}{}
	}
	for _, route := range want {
		if _, ok := gotSet[route]; !ok {
			missing = append(missing, route)
		}
	}

	wantSet := make(map[string]struct{}, len(want))
	for _, route := range want {
		wantSet[route] = struct{}{}
	}
	for _, route := range got {
		if _, ok := wantSet[route]; !ok {
			unexpected = append(unexpected, route)
		}
	}
	return missing, unexpected
}

func joinRoutes(routes []string) string {
	if len(routes) == 0 {
		return "(none)"
	}
	return strings.Join(routes, "\n")
}
