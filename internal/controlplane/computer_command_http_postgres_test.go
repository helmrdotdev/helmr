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
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

// commandHTTP serves NewServer to an API key that may create Commands in the
// Session fixture's Environment.
type commandHTTP struct {
	sessiontest.Fixture
	handler http.Handler
	key     string
}

func newCommandHTTP(t *testing.T, configure ...func(*ServerConfig)) commandHTTP {
	t.Helper()
	f := sessiontest.New(t, 1)
	return commandHTTP{
		Fixture: f,
		handler: newPostgresServer(t, f.Pool, configure...),
		key:     issueEnvironmentAPIKey(t, f.Pool, f.OrgID, f.ProjectID, f.EnvironmentID, auth.PermissionComputerCommandCreate),
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
	if err := json.Unmarshal(c.expect(t, c.key, http.MethodPost, "/v1/computers/"+c.ComputerIDs[0].String()+"/exec", body, http.StatusAccepted, ""), &receipt); err != nil {
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
	if err := c.Pool.QueryRow(t.Context(), `SELECT argv,cwd,env->>'LANG',stdin,timeout_ms FROM computer_commands WHERE id=$1`, admitted.CommandID).Scan(&argv, &cwd, &language, &stdin, &timeoutMS); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, []string{"printf", "", "hello world"}) || cwd != "/workspace/repo" || language != "C.UTF-8" || string(stdin) != "hello" || timeoutMS != 7000 {
		t.Fatalf("persisted launch argv=%q cwd=%q env=%q stdin=%q timeout=%d", argv, cwd, language, stdin, timeoutMS)
	}

	if _, err := c.Pool.Exec(t.Context(), `
		UPDATE computer_commands
		   SET status = 'failed', failure_reason='dispatch_failed',
		       revision = revision + 1,
		       terminal_at = now(),
		       terminal_reason_code = 'computer_command_assignment_timed_out',
		       error = '{"code":"internal detail that must not be public"}'::jsonb,
		       updated_at = now()
		 WHERE id = $1
	`, admitted.CommandID); err != nil {
		t.Fatal(err)
	}

	path := "/v1/computers/" + c.ComputerIDs[0].String() + "/exec"
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
	if err := c.Pool.QueryRow(t.Context(), `
		SELECT count(*) FROM computer_commands WHERE computer_id = $1
	`, c.ComputerIDs[0]).Scan(&processCount); err != nil {
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
	path := "/v1/computers/" + c.ComputerIDs[0].String() + "/exec"
	c.expect(t, c.key, http.MethodPost, path, `{"command":[],"idempotency_key":"empty"}`, http.StatusBadRequest, "invalid_computer_command")
	c.expect(t, c.key, http.MethodPost, path, `{"command":["true"],"env":{"API_TOKEN":"x"},"idempotency_key":"secret-override"}`, http.StatusBadRequest, "invalid_computer_command")
	c.expect(t, c.key, http.MethodPost, path, `{"command":["true"],"idempotency_key":"stdin","stdin_base64":"`+base64.StdEncoding.EncodeToString(make([]byte, command.MaxStdinBytes+1))+`"}`, http.StatusRequestEntityTooLarge, "computer_stdin_too_large")
	c.expect(t, c.key, http.MethodPost, path, `{"command":["`+strings.Repeat("x", 64<<10+1)+`"],"idempotency_key":"large"}`, http.StatusRequestEntityTooLarge, "computer_command_request_too_large")
	c.expect(t, c.key, http.MethodPost, path, `{"command":["true"]}`, http.StatusBadRequest, "invalid_idempotency_key")
	c.expect(t, c.key, http.MethodPost, "/v1/computers/"+uuid.NewV7().String()+"/exec", `{"command":["true"],"idempotency_key":"absent"}`, http.StatusNotFound, "computer_not_found")
	var admitted int
	if err := c.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_commands`).Scan(&admitted); err != nil || admitted != 0 {
		t.Fatalf("admitted=%d err=%v", admitted, err)
	}
}

func TestExecuteComputerHTTPReplaySurvivesComputerDeletion(t *testing.T) {
	c := newCommandHTTP(t)
	path := "/v1/computers/" + c.ComputerIDs[0].String() + "/exec"
	body := `{"command":["true"],"idempotency_key":"delete-replay"}`
	accepted := c.exec(t, body)
	if _, err := c.Pool.Exec(t.Context(), `UPDATE computer_commands
 SET status='failed',failure_reason='dispatch_failed',terminal_at=now(),terminal_reason_code='computer_command_assignment_timed_out' WHERE id=$1`, accepted.CommandID); err != nil {
		t.Fatal(err)
	}
	if _, err := computer.Delete(t.Context(), c.Pool, computer.Deletion{
		Scope:      computer.Scope{OrgID: c.OrgID, ProjectID: c.ProjectID, EnvironmentID: c.EnvironmentID},
		ComputerID: c.ComputerIDs[0], IdempotencyKey: "delete-after-exec",
	}); err != nil {
		t.Fatal(err)
	}
	if deleted, err := db.New(c.Pool).FinalizeDeletingComputers(t.Context(), 100); err != nil || len(deleted) != 1 {
		t.Fatalf("finalize deleted Computer=%v, %v", deleted, err)
	}
	if replay := c.exec(t, body); replay != accepted {
		t.Fatalf("replay after deletion = %+v, want %+v", replay, accepted)
	}
	c.expect(t, c.key, http.MethodPost, path, strings.Replace(body, "true", "false", 1), http.StatusConflict, "idempotency_conflict")
	c.expect(t, c.key, http.MethodPost, path, strings.Replace(body, "delete-replay", "new-work", 1), http.StatusNotFound, "computer_not_found")
	if _, err := c.Pool.Exec(t.Context(), `UPDATE idempotency_claims SET receipt_expires_at=now()-interval '1 day'
 WHERE id=(SELECT claim_id FROM computer_commands WHERE id=$1)`, accepted.CommandID); err != nil {
		t.Fatal(err)
	}
	if pruned, err := db.New(c.Pool).PruneExpiredIdempotencyReceipts(t.Context(), 100); err != nil || pruned != 1 {
		t.Fatalf("prune receipt=%d, %v", pruned, err)
	}
	c.expect(t, c.key, http.MethodPost, path, body, http.StatusGone, "operation_expired")
	c.expect(t, c.key, http.MethodPost, path, strings.Replace(body, "true", "false", 1), http.StatusConflict, "idempotency_conflict")
	var count int
	if err := c.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_commands WHERE computer_id=$1`, c.ComputerIDs[0]).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retained exec count=%d, %v", count, err)
	}
}

func TestExecuteComputerHTTPRejectsLostStateWithoutRetry(t *testing.T) {
	c := newCommandHTTP(t)
	if _, err := c.Pool.Exec(t.Context(), `UPDATE computers SET status='recovery_required',dirty_state='dirty_state_lost',desired_state='stopped',recovery_id=gen_random_uuid(),recovery_disk_version_id=head_disk_version_id,recovery_reason='worker_lost',recovery_started_at=clock_timestamp() WHERE id=$1`, c.ComputerIDs[0]); err != nil {
		t.Fatal(err)
	}
	body := c.expect(t, c.key, http.MethodPost, "/v1/computers/"+c.ComputerIDs[0].String()+"/exec", `{"command":["true"],"idempotency_key":"lost-state"}`, http.StatusConflict, "computer_recovery_required")
	if got := decodeHTTPError(t, body); got.Message != "computer requires recovery" || strings.Contains(string(body), `"retryable":true`) {
		t.Fatalf("lost state error=%s", body)
	}
	var admitted int
	if err := c.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_commands`).Scan(&admitted); err != nil || admitted != 0 {
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
	if info.ID != admitted.CommandID || info.ComputerID != c.ComputerIDs[0].String() || info.Status != "pending" || info.Outcome != nil {
		t.Fatalf("info = %+v", info)
	}
	viewer := issueEnvironmentAPIKey(t, c.Pool, c.OrgID, c.ProjectID, c.EnvironmentID, auth.PermissionComputersRead)
	c.expect(t, viewer, http.MethodGet, path, "", http.StatusForbidden, "permission_required")
	c.expect(t, c.key, http.MethodGet, "/v1/commands/not-a-command", "", http.StatusBadRequest, "invalid_command_reference")
	c.expect(t, c.key, http.MethodGet, "/v1/commands/"+uuid.NewV7().String(), "", http.StatusNotFound, "computer_command_not_found")
	if _, err := c.Pool.Exec(t.Context(), `UPDATE computer_commands SET status='failed',failure_reason='dispatch_failed',terminal_at=now(),
 terminal_reason_code='computer_command_assignment_timed_out',result_expires_at=now()-interval '1 day' WHERE id=$1`, admitted.CommandID); err != nil {
		t.Fatal(err)
	}
	if pruned, err := db.New(c.Pool).PruneExpiredComputerCommandResults(t.Context(), 100); err != nil || pruned != 1 {
		t.Fatalf("prune result=%d, %v", pruned, err)
	}
	c.expect(t, c.key, http.MethodGet, path, "", http.StatusGone, "command_result_expired")
}

// A Command is found only with credentials for its organization, project
// and Environment: every other scope reads, cancels and lists logs of no
// Command.
func TestCommandHTTPIsolatesEveryScopeCoordinate(t *testing.T) {
	c := newCommandHTTP(t)
	admitted := c.exec(t, `{"command":["true"],"idempotency_key":"isolated"}`)
	environment := func(orgID, projectID uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), c.Pool, `INSERT INTO environments (id, org_id, project_id, slug, name, color_hex) VALUES ($1, $2, $3, $4, 'Other', '#3366ff')`, id, orgID, projectID, "other-"+id.String())
		return id
	}
	project := func(orgID uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), c.Pool, `INSERT INTO projects (id, org_id, default_region_id, slug, name) VALUES ($1, $2, 'us-east-1', $3, 'Other')`, id, orgID, "other-"+id.String())
		return id
	}
	otherOrg := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), c.Pool, `INSERT INTO organizations (id, name, slug) VALUES ($1, 'Other', $2)`, otherOrg, "other-"+otherOrg.String())
	orgProject := project(otherOrg)
	otherProject := project(c.OrgID)
	for name, scope := range map[string][3]uuid.UUID{
		"organization": {otherOrg, orgProject, environment(otherOrg, orgProject)},
		"project":      {c.OrgID, otherProject, environment(c.OrgID, otherProject)},
		"environment":  {c.OrgID, c.ProjectID, environment(c.OrgID, c.ProjectID)},
	} {
		t.Run(name, func(t *testing.T) {
			key := issueEnvironmentAPIKey(t, c.Pool, scope[0], scope[1], scope[2], auth.PermissionComputerCommandCreate)
			path := "/v1/commands/" + admitted.CommandID
			c.expect(t, key, http.MethodGet, path, "", http.StatusNotFound, "computer_command_not_found")
			c.expect(t, key, http.MethodPost, path+"/cancel", "", http.StatusNotFound, "computer_command_not_found")
			c.expect(t, key, http.MethodGet, path+"/logs", "", http.StatusNotFound, "command_not_found")
		})
	}
	var status string
	if err := c.Pool.QueryRow(t.Context(), `SELECT status FROM computer_commands WHERE id=$1`, admitted.CommandID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("isolated Command status = %s, %v", status, err)
	}
}
