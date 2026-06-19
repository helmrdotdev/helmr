package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestWaitpointTokenRouteScopeResolvesSessionPathSlugs(t *testing.T) {
	server := &Server{db: &fakeStore{}}
	request := httptest.NewRequest(http.MethodPost, "/api/projects/main/environments/production/waitpoints/tokens", nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", "main")
	routeContext.URLParams.Add("environmentID", "production")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))

	projectID, environmentID, err := server.waitpointTokenRouteScope(request, auth.Actor{
		Kind:  auth.ActorKindSession,
		OrgID: dbtest.DefaultOrgID,
		Role:  auth.RoleOwner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if projectID.String() != testProjectIDString() || environmentID.String() != testEnvironmentIDString() {
		t.Fatalf("scope = project %s environment %s", projectID, environmentID)
	}
}
