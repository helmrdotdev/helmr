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
DELETE /api/projects/{projectID}/environments/{environmentID}/waitpoint-policies/{name}
DELETE /api/schedules/{id}
DELETE /api/secrets/{name}
DELETE /api/waitpoint-policies/{name}
GET /api/auth/device/status
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
GET /api/projects/{projectID}/environments/{environmentID}/deployments/current
GET /api/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}
GET /api/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}/events
GET /api/projects/{projectID}/environments/{environmentID}/runs
GET /api/projects/{projectID}/environments/{environmentID}/runs/counts
GET /api/projects/{projectID}/environments/{environmentID}/runs/{id}
GET /api/projects/{projectID}/environments/{environmentID}/runs/{id}/events
GET /api/projects/{projectID}/environments/{environmentID}/runs/{id}/logs
GET /api/projects/{projectID}/environments/{environmentID}/schedules
GET /api/projects/{projectID}/environments/{environmentID}/schedules/{id}
GET /api/projects/{projectID}/environments/{environmentID}/secrets
GET /api/projects/{projectID}/environments/{environmentID}/secrets/{name}
GET /api/projects/{projectID}/environments/{environmentID}/waitpoint-policies
GET /api/projects/{projectID}/environments/{environmentID}/waitpoint-policies/{name}
GET /api/runs
GET /api/runs/counts
GET /api/runs/{id}
GET /api/runs/{id}/events
GET /api/runs/{id}/logs
GET /api/schedules
GET /api/schedules/{id}
GET /api/secrets
GET /api/secrets/{name}
GET /api/waitpoint-policies
GET /api/waitpoint-policies/{name}
GET /api/worker/status
GET /healthz
GET /readyz
GET /waitpoints/respond
PATCH /api/members/{userID}
PATCH /api/projects/{projectID}
PATCH /api/projects/{projectID}/environments/{environmentID}
PATCH /api/projects/{projectID}/environments/{environmentID}/waitpoint-policies/{name}
PATCH /api/waitpoint-policies/{name}
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
POST /api/projects/{projectID}/environments/{environmentID}/runs
POST /api/projects/{projectID}/environments/{environmentID}/runs/{id}/cancel
POST /api/projects/{projectID}/environments/{environmentID}/runs/{id}/replay
POST /api/projects/{projectID}/environments/{environmentID}/schedules
POST /api/projects/{projectID}/environments/{environmentID}/schedules/{id}/activate
POST /api/projects/{projectID}/environments/{environmentID}/schedules/{id}/deactivate
POST /api/projects/{projectID}/environments/{environmentID}/waitpoint-policies
POST /api/projects/{projectID}/environments/{environmentID}/waitpoints
POST /api/projects/{projectID}/environments/{environmentID}/waitpoints/tokens
POST /api/projects/{projectID}/environments/{environmentID}/waitpoints/{waitpointID}/respond
POST /api/runs
POST /api/runs/{id}/cancel
POST /api/runs/{id}/replay
POST /api/schedules
POST /api/schedules/{id}/activate
POST /api/schedules/{id}/deactivate
POST /api/waitpoint-policies
POST /api/waitpoints
POST /api/waitpoints/tokens
POST /api/waitpoints/tokens/{tokenID}/respond
POST /api/waitpoints/{waitpointID}/respond
POST /api/worker/activate
POST /api/worker/auth/token
POST /api/worker/deployments/complete
POST /api/worker/deployments/lease
POST /api/worker/drain
POST /api/worker/register
POST /api/worker/sessions/checkpoints/failed
POST /api/worker/sessions/checkpoints/ready
POST /api/worker/sessions/events
POST /api/worker/sessions/lease
POST /api/worker/sessions/log-entries
POST /api/worker/sessions/logs
POST /api/worker/sessions/release
POST /api/worker/sessions/renew
POST /api/worker/sessions/restores/ack
POST /api/worker/sessions/start
POST /api/worker/sessions/waitpoints
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
