package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
)

func TestExecuteComputerHTTPPostgresReturnsAdmissionAndTerminalReplay(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	principal := computerCommandHTTPPrincipal(fixture.orgID, fixture.projectID, fixture.environmentID)
	body := `{"command":["printf","","hello world"],"cwd":"/workspace/repo","env":{"LANG":"C.UTF-8"},"stdin_base64":"aGVsbG8=","timeout":"7s","idempotency_key":"http-exec-1"}`

	firstRecorder := httptest.NewRecorder()
	fixture.server.executeComputerHTTP(
		firstRecorder,
		computerCommandHTTPPostRequest(body, fixture.computerRefs[0], principal),
	)
	if firstRecorder.Code != http.StatusAccepted {
		t.Fatalf("admission status=%d body=%s", firstRecorder.Code, firstRecorder.Body.String())
	}
	var admitted api.CommandReceipt
	if err := json.Unmarshal(firstRecorder.Body.Bytes(), &admitted); err != nil {
		t.Fatal(err)
	}
	if err := ids.Validate(admitted.CommandID); err != nil {
		t.Fatalf("process ID: %v", err)
	}

	var argv []string
	var cwd, language string
	var stdin []byte
	var timeoutMS int64
	if err := fixture.pool.QueryRow(t.Context(), `SELECT argv,cwd,env->>'LANG',stdin,timeout_ms FROM computer_commands WHERE id=$1`, admitted.CommandID).Scan(&argv, &cwd, &language, &stdin, &timeoutMS); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, []string{"printf", "", "hello world"}) || cwd != "/workspace/repo" || language != "C.UTF-8" || string(stdin) != "hello" || timeoutMS != 7000 {
		t.Fatalf("persisted launch argv=%q cwd=%q env=%q stdin=%q timeout=%d", argv, cwd, language, stdin, timeoutMS)
	}

	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE computer_commands
		   SET status = 'failed', failure_reason='placement_failed',
		       revision = revision + 1,
		       terminal_at = now(),
		       terminal_reason_code = 'computer_command_placement_timed_out',
		       error = '{"code":"internal detail that must not be public"}'::jsonb,
		       updated_at = now()
		 WHERE id = $1
	`, admitted.CommandID); err != nil {
		t.Fatal(err)
	}

	replayRecorder := httptest.NewRecorder()
	fixture.server.executeComputerHTTP(
		replayRecorder,
		computerCommandHTTPPostRequest(body, fixture.computerRefs[0], principal),
	)
	if replayRecorder.Code != http.StatusAccepted {
		t.Fatalf("replay status=%d body=%s", replayRecorder.Code, replayRecorder.Body.String())
	}
	var replayed api.CommandReceipt
	if err := json.Unmarshal(replayRecorder.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.CommandID != admitted.CommandID {
		t.Fatalf("replay=%+v admission=%+v", replayed, admitted)
	}

	if strings.Contains(replayRecorder.Body.String(), "internal detail") {
		t.Fatalf("replay leaked durable error: %s", replayRecorder.Body.String())
	}

	var processCount int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM computer_commands WHERE computer_id = $1
	`, fixture.computerIDs[0]).Scan(&processCount); err != nil {
		t.Fatal(err)
	}
	if processCount != 1 {
		t.Fatalf("process count = %d, want idempotent replay", processCount)
	}
}

func computerCommandHTTPPostRequest(body string, computerID string, principal auth.Actor) *http.Request {
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/computers/%s/exec", computerID), strings.NewReader(body))
	route := chi.NewRouteContext()
	route.URLParams.Add("computerID", computerID)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}

func TestExecuteComputerHTTPReplaySurvivesComputerDeletion(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	principal := computerCommandHTTPPrincipal(fixture.orgID, fixture.projectID, fixture.environmentID)
	body := `{"command":["true"],"idempotency_key":"delete-replay"}`
	invoke := func(body string, want int, code string) string {
		t.Helper()
		response := httptest.NewRecorder()
		fixture.server.executeComputerHTTP(response, computerCommandHTTPPostRequest(body, fixture.computerRefs[0], principal))
		if response.Code != want || (code != "" && !strings.Contains(response.Body.String(), code)) {
			t.Fatalf("exec response=%d %s, want %d/%s", response.Code, response.Body.String(), want, code)
		}
		return response.Body.String()
	}
	var accepted api.CommandReceipt
	if err := json.Unmarshal([]byte(invoke(body, http.StatusAccepted, "")), &accepted); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE computer_commands
 SET status='failed',failure_reason='placement_failed',terminal_at=now(),terminal_reason_code='computer_command_placement_timed_out' WHERE id=$1`, accepted.CommandID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.server.deleteComputer(t.Context(), computerDeleteRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		ComputerID: fixture.computerIDs[0], IdempotencyKey: "delete-after-exec",
	}); err != nil {
		t.Fatal(err)
	}
	if deleted, err := fixture.server.db.FinalizeDeletingComputers(t.Context(), 100); err != nil || len(deleted) != 1 {
		t.Fatalf("finalize deleted Computer=%v, %v", deleted, err)
	}
	invoke(body, http.StatusAccepted, accepted.CommandID)
	invoke(strings.Replace(body, "true", "false", 1), http.StatusConflict, "idempotency_conflict")
	invoke(strings.Replace(body, "delete-replay", "new-work", 1), http.StatusNotFound, "computer_not_found")
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE idempotency_claims SET receipt_expires_at=now()-interval '1 day'
 WHERE id=(SELECT claim_id FROM computer_commands WHERE id=$1)`, accepted.CommandID); err != nil {
		t.Fatal(err)
	}
	if pruned, err := fixture.server.db.PruneExpiredIdempotencyReceipts(t.Context(), 100); err != nil || pruned != 1 {
		t.Fatalf("prune receipt=%d, %v", pruned, err)
	}
	invoke(body, http.StatusGone, "operation_expired")
	invoke(strings.Replace(body, "true", "false", 1), http.StatusConflict, "idempotency_conflict")
	var count int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_commands WHERE computer_id=$1`, fixture.computerIDs[0]).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retained exec count=%d, %v", count, err)
	}
}

func TestExecuteComputerHTTPRejectsCaptureFailureWithoutRetry(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE computers SET dirty_state='capture_failed',desired_state='stopped' WHERE id=$1`, fixture.computerIDs[0]); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	fixture.server.executeComputerHTTP(response, computerCommandHTTPPostRequest(`{"command":["true"],"idempotency_key":"failed-capture"}`, fixture.computerRefs[0], computerCommandHTTPPrincipal(fixture.orgID, fixture.projectID, fixture.environmentID)))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"computer_recovery_required"`) || strings.Contains(response.Body.String(), `"retryable":true`) {
		t.Fatalf("capture failure response=%d %s", response.Code, response.Body.String())
	}
	var admitted int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_commands`).Scan(&admitted); err != nil || admitted != 0 {
		t.Fatalf("admitted=%d err=%v", admitted, err)
	}
}
