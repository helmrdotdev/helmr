package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
)

func TestCancelRunHTTPAcceptsExactActorStopAndReplaysReceipt(t *testing.T) {
	f := newActorStartPostgresFixture(t, 1)
	started, err := f.server.startActor(t.Context(), f.request(0, nil, "actor-cancel"))
	if err != nil {
		t.Fatal(err)
	}
	principal := auth.Actor{OrgID: f.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper, ProjectID: f.projectID.String(), EnvironmentID: f.environmentID.String(), Permissions: []auth.Permission{auth.PermissionRunsManage}}
	cancel := func() *httptest.ResponseRecorder {
		request := runCancellationRequest(t, started.BootRunID.String(), principal)
		request.Body = io.NopCloser(strings.NewReader(`{"idempotency_key":"stop-init"}`))
		w := httptest.NewRecorder()
		f.server.cancelRunHTTP(w, request)
		return w
	}
	w := cancel()
	if w.Code != http.StatusAccepted {
		t.Fatalf("cancel=%d %s", w.Code, w.Body.String())
	}
	var receipt api.ActorRunCancellationReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "accepted" || receipt.RunID != started.BootRunID.String() || receipt.SessionID != started.SessionID.String() || receipt.ID == "" || receipt.HoldID == "" {
		t.Fatalf("receipt=%+v", receipt)
	}
	var hold uuid.UUID
	var current *uuid.UUID
	var status string
	if err := f.pool.QueryRow(t.Context(), `SELECT dispatch_hold_id,current_run_id,status FROM sessions WHERE id=$1`, started.SessionID).Scan(&hold, &current, &status); err != nil {
		t.Fatal(err)
	}
	if hold.String() != receipt.HoldID || status != "open" {
		t.Fatalf("Session hold=%v current=%v status=%s", hold, current, status)
	}
	replay := cancel()
	if replay.Code != http.StatusAccepted || replay.Body.String() != w.Body.String() {
		t.Fatalf("replay=%d %s", replay.Code, replay.Body.String())
	}
}

func TestCancelRunHTTPRejectsActiveTurnWithoutMutatingItsAuthority(t *testing.T) {
	f := newActorCheckpointFixture(t)
	scope := f.receiveTurn(t, 1)
	principal := auth.Actor{OrgID: f.OrgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper, ProjectID: f.ProjectID.String(), EnvironmentID: f.EnvironmentID.String(), Permissions: []auth.Permission{auth.PermissionRunsManage}}
	request := runCancellationRequest(t, f.runID.String(), principal)
	request.Body = io.NopCloser(strings.NewReader(`{"idempotency_key":"stale-run-only-cancel"}`))
	w := httptest.NewRecorder()
	f.server.cancelRunHTTP(w, request)
	if w.Code != http.StatusConflict {
		t.Fatalf("active cancel=%d %s", w.Code, w.Body.String())
	}
	var active uuid.UUID
	var hold *uuid.UUID
	var interrupted bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT s.active_turn_id,s.dispatch_hold_id,t.interrupt_requested_at IS NOT NULL FROM sessions s JOIN session_turns t ON t.id=s.active_turn_id WHERE s.id=$1`, f.sessionID).Scan(&active, &hold, &interrupted); err != nil {
		t.Fatal(err)
	}
	if active != scope.TurnID || hold != nil || interrupted {
		t.Fatalf("active=%s hold=%v interrupted=%v", active, hold, interrupted)
	}
}

func TestCancelRunHTTPDeniesBeforeRunValidation(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/runs/not-a-run/cancel", nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("runID", "not-a-run")
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, auth.Actor{
		Kind: auth.ActorKindAPIKey, OrgID: uuid.NewV7(),
		ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String(),
	})
	recorder := httptest.NewRecorder()

	(&Server{}).cancelRunHTTP(recorder, request.WithContext(ctx))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeHTTPError(t, recorder.Body.Bytes())
	if response.Code != "permission_required" {
		t.Fatalf("unexpected response: %s", recorder.Body.String())
	}
}

func runCancellationRequest(
	t *testing.T,
	runID string,
	principal auth.Actor,
) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/runs/"+runID+"/cancel", nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("runID", runID)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}
