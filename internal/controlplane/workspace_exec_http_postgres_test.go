package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
)

func TestExecuteWorkspaceHTTPPostgresReturnsAdmissionAndTerminalReplay(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	principal := workspaceExecHTTPPrincipal(fixture.orgID, fixture.projectID, fixture.environmentID)
	body := `{"command":["true"],"idempotency_key":"http-exec-1"}`

	firstRecorder := httptest.NewRecorder()
	fixture.server.executeWorkspaceHTTP(
		firstRecorder,
		workspaceExecHTTPPostRequest(body, fixture.workspaceRefs[0], principal),
	)
	if firstRecorder.Code != http.StatusAccepted {
		t.Fatalf("admission status=%d body=%s", firstRecorder.Code, firstRecorder.Body.String())
	}
	var admitted api.WorkspaceExecProcess
	if err := json.Unmarshal(firstRecorder.Body.Bytes(), &admitted); err != nil {
		t.Fatal(err)
	}
	if err := ids.Validate(admitted.ProcessID); err != nil {
		t.Fatalf("process ID: %v", err)
	}
	if admitted.Status != api.WorkspaceExecProcessStatusPending || admitted.ExitCode != nil || admitted.Error != nil {
		t.Fatalf("admission = %+v", admitted)
	}

	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE workspace_processes
		   SET state = 'failed',
		       state_version = state_version + 1,
		       terminal_at = now(),
		       terminal_reason_code = 'workspace_exec_placement_timed_out',
		       error = '{"code":"internal detail that must not be public"}'::jsonb,
		       updated_at = now()
		 WHERE id = $1
	`, admitted.ProcessID); err != nil {
		t.Fatal(err)
	}

	replayRecorder := httptest.NewRecorder()
	fixture.server.executeWorkspaceHTTP(
		replayRecorder,
		workspaceExecHTTPPostRequest(body, fixture.workspaceRefs[0], principal),
	)
	if replayRecorder.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", replayRecorder.Code, replayRecorder.Body.String())
	}
	var replayed api.WorkspaceExecProcess
	if err := json.Unmarshal(replayRecorder.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.ProcessID != admitted.ProcessID || replayed.Status != api.WorkspaceExecProcessStatusFailed ||
		replayed.Error == nil || replayed.Error.TerminalReasonCode != "workspace_exec_placement_timed_out" {
		t.Fatalf("replay = %+v, admission = %+v", replayed, admitted)
	}
	if strings.Contains(replayRecorder.Body.String(), "internal detail") {
		t.Fatalf("replay leaked durable error: %s", replayRecorder.Body.String())
	}

	var processCount int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM workspace_processes WHERE workspace_id = $1
	`, fixture.workspaceIDs[0]).Scan(&processCount); err != nil {
		t.Fatal(err)
	}
	if processCount != 1 {
		t.Fatalf("process count = %d, want idempotent replay", processCount)
	}
}

func workspaceExecHTTPPostRequest(body string, workspaceID string, principal auth.Actor) *http.Request {
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/workspaces/%s/exec", workspaceID), strings.NewReader(body))
	route := chi.NewRouteContext()
	route.URLParams.Add("workspaceID", workspaceID)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}
