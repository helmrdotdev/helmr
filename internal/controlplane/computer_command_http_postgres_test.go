package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/ids"
)

// commandHTTP serves NewServer to an API key that may create Commands in the
// actor start fixture's Environment.
type commandHTTP struct {
	actorStartPostgresFixture
	handler http.Handler
	key     string
}

func newCommandHTTP(t *testing.T, configure ...func(*ServerConfig)) commandHTTP {
	t.Helper()
	f := newActorStartPostgresFixture(t, 1)
	return commandHTTP{
		actorStartPostgresFixture: f,
		handler:                   newPostgresServer(t, f.pool, configure...),
		key:                       issueEnvironmentAPIKey(t, f.pool, f.orgID, f.projectID, f.environmentID, auth.PermissionComputerCommandCreate),
	}
}

// expect serves the request with key, requires the status and, when code is
// set, the error code, and returns the response body.
func (c commandHTTP) expect(t *testing.T, key, method, path, body string, status int, code string) []byte {
	t.Helper()
	response := serveAPIKey(c.handler, method, path, key, body)
	if response.Code != status {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, response.Code, status, response.Body.String())
	}
	if code != "" {
		if got := decodeHTTPError(t, response.Body.Bytes()); got.Code != code {
			t.Fatalf("%s %s code = %q, want %q", method, path, got.Code, code)
		}
	}
	return response.Body.Bytes()
}

// exec admits a Command through the exec route and returns its receipt.
func (c commandHTTP) exec(t *testing.T, body string) api.CommandReceipt {
	t.Helper()
	var receipt api.CommandReceipt
	if err := json.Unmarshal(c.expect(t, c.key, http.MethodPost, "/v1/computers/"+c.computerRefs[0]+"/exec", body, http.StatusAccepted, ""), &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestExecuteComputerHTTPPostgresReturnsAdmissionAndTerminalReplay(t *testing.T) {
	c := newCommandHTTP(t)
	body := `{"command":["printf","","hello world"],"cwd":"/workspace/repo","env":{"LANG":"C.UTF-8"},"stdin_base64":"aGVsbG8=","timeout":"7s","idempotency_key":"http-exec-1"}`
	admitted := c.exec(t, body)
	if err := ids.Validate(admitted.CommandID); err != nil {
		t.Fatalf("process ID: %v", err)
	}

	var argv []string
	var cwd, language string
	var stdin []byte
	var timeoutMS int64
	if err := c.pool.QueryRow(t.Context(), `SELECT argv,cwd,env->>'LANG',stdin,timeout_ms FROM computer_commands WHERE id=$1`, admitted.CommandID).Scan(&argv, &cwd, &language, &stdin, &timeoutMS); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, []string{"printf", "", "hello world"}) || cwd != "/workspace/repo" || language != "C.UTF-8" || string(stdin) != "hello" || timeoutMS != 7000 {
		t.Fatalf("persisted launch argv=%q cwd=%q env=%q stdin=%q timeout=%d", argv, cwd, language, stdin, timeoutMS)
	}

	if _, err := c.pool.Exec(t.Context(), `
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

	path := "/v1/computers/" + c.computerRefs[0] + "/exec"
	replay := c.expect(t, c.key, http.MethodPost, path, body, http.StatusAccepted, "")
	var replayed api.CommandReceipt
	if err := json.Unmarshal(replay, &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.CommandID != admitted.CommandID {
		t.Fatalf("replay=%+v admission=%+v", replayed, admitted)
	}
	if strings.Contains(string(replay), "internal detail") {
		t.Fatalf("replay leaked durable error: %s", replay)
	}

	var processCount int
	if err := c.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM computer_commands WHERE computer_id = $1
	`, c.computerIDs[0]).Scan(&processCount); err != nil {
		t.Fatal(err)
	}
	if processCount != 1 {
		t.Fatalf("process count = %d, want idempotent replay", processCount)
	}
}

// The exec route reports rejected requests with their codes before any
// Command is admitted.
func TestExecuteComputerHTTPRejectsInput(t *testing.T) {
	c := newCommandHTTP(t)
	path := "/v1/computers/" + c.computerRefs[0] + "/exec"
	c.expect(t, c.key, http.MethodPost, path, `{"command":[],"idempotency_key":"empty"}`, http.StatusBadRequest, "invalid_computer_command")
	c.expect(t, c.key, http.MethodPost, path, `{"command":["true"],"env":{"API_TOKEN":"x"},"idempotency_key":"secret-override"}`, http.StatusBadRequest, "invalid_computer_command")
	c.expect(t, c.key, http.MethodPost, path, `{"command":["true"],"idempotency_key":"stdin","stdin_base64":"`+base64.StdEncoding.EncodeToString(make([]byte, command.MaxStdinBytes+1))+`"}`, http.StatusRequestEntityTooLarge, "computer_stdin_too_large")
	c.expect(t, c.key, http.MethodPost, path, `{"command":["`+strings.Repeat("x", 64<<10+1)+`"],"idempotency_key":"large"}`, http.StatusRequestEntityTooLarge, "computer_command_request_too_large")
	c.expect(t, c.key, http.MethodPost, path, `{"command":["true"]}`, http.StatusBadRequest, "invalid_idempotency_key")
	c.expect(t, c.key, http.MethodPost, "/v1/computers/"+uuid.NewV7().String()+"/exec", `{"command":["true"],"idempotency_key":"absent"}`, http.StatusNotFound, "computer_not_found")
	var admitted int
	if err := c.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_commands`).Scan(&admitted); err != nil || admitted != 0 {
		t.Fatalf("admitted=%d err=%v", admitted, err)
	}
}

func TestExecuteComputerHTTPReplaySurvivesComputerDeletion(t *testing.T) {
	c := newCommandHTTP(t)
	path := "/v1/computers/" + c.computerRefs[0] + "/exec"
	body := `{"command":["true"],"idempotency_key":"delete-replay"}`
	accepted := c.exec(t, body)
	if _, err := c.pool.Exec(t.Context(), `UPDATE computer_commands
 SET status='failed',failure_reason='placement_failed',terminal_at=now(),terminal_reason_code='computer_command_placement_timed_out' WHERE id=$1`, accepted.CommandID); err != nil {
		t.Fatal(err)
	}
	if _, err := computer.Delete(t.Context(), c.pool, computer.Deletion{
		Scope:      computer.Scope{OrgID: c.orgID, ProjectID: c.projectID, EnvironmentID: c.environmentID},
		ComputerID: c.computerIDs[0], IdempotencyKey: "delete-after-exec",
	}); err != nil {
		t.Fatal(err)
	}
	if deleted, err := c.server.db.FinalizeDeletingComputers(t.Context(), 100); err != nil || len(deleted) != 1 {
		t.Fatalf("finalize deleted Computer=%v, %v", deleted, err)
	}
	if replay := c.exec(t, body); replay != accepted {
		t.Fatalf("replay after deletion = %+v, want %+v", replay, accepted)
	}
	c.expect(t, c.key, http.MethodPost, path, strings.Replace(body, "true", "false", 1), http.StatusConflict, "idempotency_conflict")
	c.expect(t, c.key, http.MethodPost, path, strings.Replace(body, "delete-replay", "new-work", 1), http.StatusNotFound, "computer_not_found")
	if _, err := c.pool.Exec(t.Context(), `UPDATE idempotency_claims SET receipt_expires_at=now()-interval '1 day'
 WHERE id=(SELECT claim_id FROM computer_commands WHERE id=$1)`, accepted.CommandID); err != nil {
		t.Fatal(err)
	}
	if pruned, err := c.server.db.PruneExpiredIdempotencyReceipts(t.Context(), 100); err != nil || pruned != 1 {
		t.Fatalf("prune receipt=%d, %v", pruned, err)
	}
	c.expect(t, c.key, http.MethodPost, path, body, http.StatusGone, "operation_expired")
	c.expect(t, c.key, http.MethodPost, path, strings.Replace(body, "true", "false", 1), http.StatusConflict, "idempotency_conflict")
	var count int
	if err := c.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_commands WHERE computer_id=$1`, c.computerIDs[0]).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retained exec count=%d, %v", count, err)
	}
}

func TestExecuteComputerHTTPRejectsCaptureFailureWithoutRetry(t *testing.T) {
	c := newCommandHTTP(t)
	if _, err := c.pool.Exec(t.Context(), `UPDATE computers SET dirty_state='capture_failed',desired_state='stopped' WHERE id=$1`, c.computerIDs[0]); err != nil {
		t.Fatal(err)
	}
	body := c.expect(t, c.key, http.MethodPost, "/v1/computers/"+c.computerRefs[0]+"/exec", `{"command":["true"],"idempotency_key":"failed-capture"}`, http.StatusConflict, "computer_recovery_required")
	if got := decodeHTTPError(t, body); got.Message != "computer requires recovery" || strings.Contains(string(body), `"retryable":true`) {
		t.Fatalf("capture failure error=%s", body)
	}
	var admitted int
	if err := c.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_commands`).Scan(&admitted); err != nil || admitted != 0 {
		t.Fatalf("admitted=%d err=%v", admitted, err)
	}
}

// The Command read route requires the Command permission, not Computer read
// access, and reports unknown and pruned Commands with their codes.
func TestGetComputerCommandHTTPPostgres(t *testing.T) {
	c := newCommandHTTP(t)
	admitted := c.exec(t, `{"command":["true"],"idempotency_key":"read"}`)
	path := "/v1/commands/" + admitted.CommandID
	var info api.CommandInfo
	if err := json.Unmarshal(c.expect(t, c.key, http.MethodGet, path, "", http.StatusOK, ""), &info); err != nil {
		t.Fatal(err)
	}
	if info.ID != admitted.CommandID || info.ComputerID != c.computerRefs[0] || info.Status != "pending" || info.Outcome != nil {
		t.Fatalf("info = %+v", info)
	}
	viewer := issueEnvironmentAPIKey(t, c.pool, c.orgID, c.projectID, c.environmentID, auth.PermissionComputersRead)
	c.expect(t, viewer, http.MethodGet, path, "", http.StatusForbidden, "permission_required")
	c.expect(t, c.key, http.MethodGet, "/v1/commands/not-a-command", "", http.StatusBadRequest, "invalid_command_reference")
	c.expect(t, c.key, http.MethodGet, "/v1/commands/"+uuid.NewV7().String(), "", http.StatusNotFound, "computer_command_not_found")
	if _, err := c.pool.Exec(t.Context(), `UPDATE computer_commands SET status='failed',failure_reason='placement_failed',terminal_at=now(),
 terminal_reason_code='computer_command_placement_timed_out',result_expires_at=now()-interval '1 day' WHERE id=$1`, admitted.CommandID); err != nil {
		t.Fatal(err)
	}
	if pruned, err := c.server.db.PruneExpiredComputerCommandResults(t.Context(), 100); err != nil || pruned != 1 {
		t.Fatalf("prune result=%d, %v", pruned, err)
	}
	c.expect(t, c.key, http.MethodGet, path, "", http.StatusGone, "command_result_expired")
}
