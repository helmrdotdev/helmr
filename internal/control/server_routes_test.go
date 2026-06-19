package control

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
DELETE /api/schedules/{id}
DELETE /api/secrets/{name}
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
GET /api/projects/{projectID}/environments/{environmentID}/runs/{id}/waitpoints
GET /api/projects/{projectID}/environments/{environmentID}/schedules
GET /api/projects/{projectID}/environments/{environmentID}/schedules/{id}
GET /api/projects/{projectID}/environments/{environmentID}/secrets
GET /api/projects/{projectID}/environments/{environmentID}/secrets/{name}
GET /api/projects/{projectID}/environments/{environmentID}/sessions
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/channels
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/channels/{channel}/inputs
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/channels/{channel}/outputs
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/channels/{channel}/outputs/stream
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/runs
GET /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/workspace
GET /api/projects/{projectID}/environments/{environmentID}/waitpoints/tokens
GET /api/projects/{projectID}/environments/{environmentID}/waitpoints/tokens/{tokenID}
GET /api/runs
GET /api/runs/counts
GET /api/runs/{id}
GET /api/runs/{id}/events
GET /api/runs/{id}/logs
GET /api/runs/{id}/waitpoints
GET /api/schedules
GET /api/schedules/{id}
GET /api/secrets
GET /api/secrets/{name}
GET /api/sessions
GET /api/sessions/{sessionID}
GET /api/sessions/{sessionID}/channels
GET /api/sessions/{sessionID}/channels/{channel}/inputs
GET /api/sessions/{sessionID}/channels/{channel}/outputs
GET /api/sessions/{sessionID}/channels/{channel}/outputs/stream
GET /api/sessions/{sessionID}/runs
GET /api/sessions/{sessionID}/workspace
GET /api/waitpoints/tokens
GET /api/waitpoints/tokens/{tokenID}
GET /api/worker/status
GET /healthz
GET /readyz
OPTIONS /api/sessions/{sessionID}/channels/{channel}/inputs
OPTIONS /api/sessions/{sessionID}/channels/{channel}/outputs
OPTIONS /api/sessions/{sessionID}/channels/{channel}/outputs/stream
OPTIONS /api/waitpoints/tokens/{tokenID}/complete
PATCH /api/members/{userID}
PATCH /api/projects/{projectID}
PATCH /api/projects/{projectID}/environments/{environmentID}
PATCH /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}
PATCH /api/sessions/{sessionID}
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
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/cancel
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/channels/{channel}/inputs
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/close
POST /api/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/wait
POST /api/projects/{projectID}/environments/{environmentID}/tasks/{taskID}/start
POST /api/projects/{projectID}/environments/{environmentID}/tasks/{taskID}/start-and-wait
POST /api/projects/{projectID}/environments/{environmentID}/waitpoints/tokens
POST /api/public-access-tokens
POST /api/runs/{id}/cancel
POST /api/schedules
POST /api/schedules/{id}/activate
POST /api/schedules/{id}/deactivate
POST /api/sessions/{sessionID}/cancel
POST /api/sessions/{sessionID}/channels/{channel}/inputs
POST /api/sessions/{sessionID}/close
POST /api/sessions/{sessionID}/wait
POST /api/tasks/{taskID}/start
POST /api/tasks/{taskID}/start-and-wait
POST /api/waitpoints/tokens
POST /api/waitpoints/tokens/{tokenID}/callback/{callbackSecret}
POST /api/waitpoints/tokens/{tokenID}/complete
POST /api/worker/activate
POST /api/worker/auth/token
POST /api/worker/deployments/complete
POST /api/worker/deployments/lease
POST /api/worker/drain
POST /api/worker/leases/channels
POST /api/worker/leases/checkpoints/failed
POST /api/worker/leases/checkpoints/ready
POST /api/worker/leases/lease
POST /api/worker/leases/log-entries
POST /api/worker/leases/logs
POST /api/worker/leases/metadata
POST /api/worker/leases/release
POST /api/worker/leases/renew
POST /api/worker/leases/restores/ack
POST /api/worker/leases/start
POST /api/worker/leases/waitpoint-tokens
POST /api/worker/leases/waitpoints
POST /api/worker/register
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

func TestWaitpointTokenCompleteOptionsCORS(t *testing.T) {
	handler := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	req := httptest.NewRequest(http.MethodOptions, "/api/waitpoints/tokens/00000000-0000-0000-0000-000000000001/complete", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("access-control-allow-origin") != "*" {
		t.Fatalf("allow-origin = %q", rec.Header().Get("access-control-allow-origin"))
	}
	if !strings.Contains(rec.Header().Get("access-control-allow-headers"), "authorization") {
		t.Fatalf("allow-headers = %q", rec.Header().Get("access-control-allow-headers"))
	}
	if !strings.Contains(rec.Header().Get("access-control-allow-headers"), "Helmr-API-Version") {
		t.Fatalf("allow-headers = %q", rec.Header().Get("access-control-allow-headers"))
	}
	if !strings.Contains(rec.Header().Get("access-control-allow-headers"), "Helmr-SDK-Version") {
		t.Fatalf("allow-headers = %q", rec.Header().Get("access-control-allow-headers"))
	}
}

func TestSessionChannelRecordsOptionsCORS(t *testing.T) {
	handler := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	req := httptest.NewRequest(http.MethodOptions, "/api/sessions/00000000-0000-0000-0000-000000000001/channels/events/outputs", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("access-control-allow-origin") != "*" {
		t.Fatalf("allow-origin = %q", rec.Header().Get("access-control-allow-origin"))
	}
	if !strings.Contains(rec.Header().Get("access-control-allow-methods"), "GET") || !strings.Contains(rec.Header().Get("access-control-allow-methods"), "POST") {
		t.Fatalf("allow-methods = %q", rec.Header().Get("access-control-allow-methods"))
	}
	for _, header := range []string{"authorization", "content-type", "idempotency-key", "Helmr-API-Version", "Helmr-SDK-Version"} {
		if !strings.Contains(rec.Header().Get("access-control-allow-headers"), header) {
			t.Fatalf("allow-headers missing %s: %q", header, rec.Header().Get("access-control-allow-headers"))
		}
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
