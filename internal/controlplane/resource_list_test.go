package controlplane

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"
)

func TestDeploymentListCursorRoundTripAndScope(t *testing.T) {
	id := uuid.NewV7()
	raw, err := encodeDeploymentListCursor(deploymentListCursor{
		ProjectID: "project-1", EnvironmentID: "environment-1",
		CreatedAt: time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC), ID: id.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/deployments?limit=25&cursor="+raw, nil)
	limit, cursor, err := parseDeploymentListQuery(request, "project-1", "environment-1")
	if err != nil {
		t.Fatal(err)
	}
	if limit != 25 || cursor == nil || cursor.ID != id.String() {
		t.Fatalf("limit=%d cursor=%+v", limit, cursor)
	}
	if _, _, err := parseDeploymentListQuery(request, "project-1", "environment-2"); err == nil {
		t.Fatal("cross-environment Deployment cursor succeeded")
	}
}

func TestDeploymentListQueryRejectsUnknownParameters(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/deployments?status=deployed", nil)
	if _, _, err := parseDeploymentListQuery(request, "project-1", "environment-1"); err == nil {
		t.Fatal("unsupported Deployment filter succeeded")
	}
}
