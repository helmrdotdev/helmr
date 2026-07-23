package control

import (
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestControlRoutesMatchCurrentProtocol(t *testing.T) {
	router := chi.NewRouter()
	server := &Server{}
	router.Route("/api", server.mountAPIRoutes)
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
GET /api/actors/{actorDeclaredID}
GET /api/actors/{actorDeclaredID}/output
GET /api/actors/{actorDeclaredID}/status
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
GET /api/projects/{projectID}/environments/{environmentID}/actors/{actorDeclaredID}
GET /api/projects/{projectID}/environments/{environmentID}/actors/{actorDeclaredID}/output
GET /api/projects/{projectID}/environments/{environmentID}/actors/{actorDeclaredID}/status
GET /api/projects/{projectID}/environments/{environmentID}/api-keys
GET /api/projects/{projectID}/environments/{environmentID}/deployments
GET /api/projects/{projectID}/environments/{environmentID}/deployments/current
GET /api/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}
GET /api/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}/events
GET /api/projects/{projectID}/environments/{environmentID}/secrets
GET /api/projects/{projectID}/environments/{environmentID}/secrets/{name}
GET /api/regions
GET /api/secrets
GET /api/secrets/{name}
GET /api/worker/status
PATCH /api/actors/{actorDeclaredID}
PATCH /api/members/{userID}
PATCH /api/projects/{projectID}
PATCH /api/projects/{projectID}/environments/{environmentID}
PATCH /api/projects/{projectID}/environments/{environmentID}/actors/{actorDeclaredID}
POST /api/actors/{actorDeclaredID}/close
POST /api/actors/{actorDeclaredID}/input
POST /api/actors/{actorDeclaredID}/start
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
POST /api/invitations
POST /api/organizations
POST /api/projects
POST /api/projects/{projectID}/environments
POST /api/projects/{projectID}/environments/{environmentID}/actors/{actorDeclaredID}/close
POST /api/projects/{projectID}/environments/{environmentID}/actors/{actorDeclaredID}/input
POST /api/projects/{projectID}/environments/{environmentID}/actors/{actorDeclaredID}/start
POST /api/projects/{projectID}/environments/{environmentID}/api-keys
POST /api/projects/{projectID}/environments/{environmentID}/deployments
POST /api/projects/{projectID}/environments/{environmentID}/secrets/{name}/revoke
POST /api/secrets/{name}/revoke
POST /api/worker/activate
POST /api/worker/auth/token
POST /api/worker/certification/renew
POST /api/worker/deployments/complete
POST /api/worker/deployments/delivery-failed
POST /api/worker/deployments/lease
POST /api/worker/deployments/reject
POST /api/worker/deployments/renew
POST /api/worker/deployments/start
POST /api/worker/drain
POST /api/worker/drain/complete
POST /api/worker/enrollment
POST /api/worker/enrollment/challenge
POST /api/worker/fence
POST /api/worker/leases/actor-inputs/send
POST /api/worker/leases/actor-turns/commit
POST /api/worker/leases/actors/complete
POST /api/worker/leases/checkpoints/failed
POST /api/worker/leases/checkpoints/ready
POST /api/worker/leases/claim
POST /api/worker/leases/discover
POST /api/worker/leases/entrypoint
POST /api/worker/leases/finalization/begin
POST /api/worker/leases/resume-release
POST /api/worker/leases/run-logs
POST /api/worker/leases/run-renew
POST /api/worker/leases/run-waits
POST /api/worker/leases/run-waits/poll
POST /api/worker/leases/run-waits/resume-ack
POST /api/worker/leases/start
POST /api/worker/leases/tasks/complete
POST /api/worker/observe
POST /api/worker/runtime-instances/closed
POST /api/worker/runtime-instances/failed
POST /api/worker/runtime-instances/ready
POST /api/worker/runtime-instances/reconcile
POST /api/worker/runtime-substrates/lookup
POST /api/worker/runtime-substrates/register
POST /api/worker/startup-recovery
PUT /api/projects/{projectID}/environments/{environmentID}/secrets/{name}
PUT /api/secrets/{name}
`), "\n")
	if !slices.IsSorted(want) {
		t.Fatal("control route snapshot must stay sorted")
	}
	if !slices.Equal(got, want) {
		t.Fatalf("control routes changed\nwant:\n%s\n\ngot:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}
