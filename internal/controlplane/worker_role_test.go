package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
)

func TestRequireWorkerRoleAllowsDrainingInFlightMutation(t *testing.T) {
	called := false
	handler := requireWorkerRole(auth.WorkerRoleRun, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/worker/v0/run/leases/renew", nil)
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, workerActor{
		State: db.WorkerInstanceStateDraining,
		Roles: []string{auth.WorkerRoleRun},
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !called {
		t.Fatalf("status = %d, called = %t; want 200, true", recorder.Code, called)
	}
}

func TestRequireWorkerRoleRejectsCrossDomainMutation(t *testing.T) {
	tests := []struct {
		name     string
		required string
		roles    []string
		path     string
	}{
		{name: "build-only token on run route", required: auth.WorkerRoleRun, roles: []string{auth.WorkerRoleBuild}, path: "/api/worker/v0/run/leases/renew"},
		{name: "run-only token on build route", required: auth.WorkerRoleBuild, roles: []string{auth.WorkerRoleRun}, path: "/api/worker/v0/build/deployments/renew"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			handler := requireWorkerRole(tt.required, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				called = true
			}))
			req := httptest.NewRequest(http.MethodPost, tt.path, nil)
			req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, workerActor{
				State: db.WorkerInstanceStateActive,
				Roles: tt.roles,
			}))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusForbidden || called {
				t.Fatalf("status = %d, called = %t; want 403, false", recorder.Code, called)
			}
		})
	}
}

func TestRequireActiveWorkerRoleRejectsDrainingClaim(t *testing.T) {
	called := false
	handler := requireActiveWorkerRole(auth.WorkerRoleRun, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/worker/v0/run/leases/claim", nil)
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, workerActor{
		State: db.WorkerInstanceStateDraining,
		Roles: []string{auth.WorkerRoleRun},
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden || called {
		t.Fatalf("status = %d, called = %t; want 403, false", recorder.Code, called)
	}
}
