package controlplane

import (
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCommandOutcomeSeparatesFailureExitAndReconciliation(t *testing.T) {
	base := db.ComputerCommand{ID: pgvalue.NewUUIDv7(), ComputerID: pgvalue.NewUUIDv7(), ComputerInstanceID: pgvalue.NewUUIDv7(), TerminalAt: pgvalue.Timestamptz(time.Now()), Status: db.ComputerCommandStatusFailed, ExitCode: pgtype.Int4{Int32: 0, Valid: true}, FailureReason: pgvalue.Text("scope_termination_failed")}
	info, err := publicCommandInfo(base)
	if err != nil || info.Outcome == nil || info.Outcome.Kind != "system_failed" || info.Outcome.Failure.Reason != "scope_termination_failed" || info.Outcome.ExitCode == nil || *info.Outcome.ExitCode != 0 || info.ProcessReconciled {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	base.ProcessReconciledAt = pgvalue.Timestamptz(time.Now())
	base.Status = db.ComputerCommandStatusExited
	info, err = publicCommandInfo(base)
	if err != nil || info.Outcome.Kind != "exited" || !info.ProcessReconciled {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	base.Status = db.ComputerCommandStatusFailed
	base.TerminalReasonCode = pgvalue.Text("database_password=secret")
	base.FailureReason = pgvalue.Text("guest_failure")
	info, err = publicCommandInfo(base)
	if err != nil || info.Outcome.Failure.Reason != "guest_failure" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}

func TestPreparationExhaustedCommandIsPlacementFailure(t *testing.T) {
	info, err := publicCommandInfo(db.ComputerCommand{ID: pgvalue.NewUUIDv7(), ComputerID: pgvalue.NewUUIDv7(), Status: db.ComputerCommandStatusFailed, TerminalAt: pgvalue.Timestamptz(time.Now()), TerminalReasonCode: pgvalue.Text("computer_preparation_exhausted"), FailureReason: pgvalue.Text("placement_failed")})
	if err != nil {
		t.Fatal(err)
	}
	if info.Outcome == nil || info.Outcome.Failure == nil || info.Outcome.Failure.Reason != "placement_failed" || info.Outcome.ExitCode != nil || !info.ProcessReconciled {
		t.Fatalf("outcome=%+v", info)
	}
}

func TestComputerCommandGETRequiresExecPermissionNotComputerRead(t *testing.T) {
	orgID := uuid.New()
	scope := auth.Scope{OrgID: orgID, ProjectID: "project", EnvironmentID: "environment"}
	viewer := auth.Principal{Kind: auth.PrincipalKindSession, OrgID: orgID, Role: auth.RoleViewer}
	if canAccessComputerCommandOutput(viewer, scope) {
		t.Fatal("Computer reader was allowed to read Computer Exec output")
	}
	createOnly := auth.Principal{
		Kind: auth.PrincipalKindAPIKey, OrgID: orgID, Role: auth.RoleDeveloper,
		ProjectID: "project", EnvironmentID: "environment",
		Permissions: []auth.Permission{auth.PermissionComputerCommandCreate},
	}
	if !canAccessComputerCommandOutput(createOnly, scope) {
		t.Fatal("create-only API key was unable to poll its Computer Exec")
	}
}

func TestNormalizeComputerCommandAppliesClosedDefaults(t *testing.T) {
	normalized, err := normalizeComputerCommand(computerCommandRequest{
		Command: []string{"printf", "", "ok"},
		Env:     map[string]string{"LANG": "C.UTF-8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.cwd != "/workspace" ||
		normalized.timeout != 5*time.Minute ||
		normalized.timeoutMS != 300000 ||
		len(normalized.command) != 3 ||
		normalized.command[1] != "" ||
		normalized.stdin == nil {
		t.Fatalf("normalized = %+v", normalized)
	}

}

func TestNonNilComputerCommandBytesClonesInput(t *testing.T) {
	if value := nonNilComputerCommandBytes(nil); value == nil || len(value) != 0 {
		t.Fatalf("empty value = %#v", value)
	}
	source := []byte("secret")
	cloned := nonNilComputerCommandBytes(source)
	clear(source)
	if string(cloned) != "secret" {
		t.Fatalf("cloned value = %q", cloned)
	}
}

func TestNormalizeComputerCommandRejectsInvalidAuthorityAndBounds(t *testing.T) {
	tooManyArgs := make([]string, computerCommandArgMaxCount+1)
	for index := range tooManyArgs {
		tooManyArgs[index] = "x"
	}
	tooManyEnv := make(map[string]string, computerCommandEnvMaxCount+1)
	for index := range computerCommandEnvMaxCount + 1 {
		tooManyEnv["K"+strings.Repeat("X", index)] = "v"
	}
	tests := []struct {
		name    string
		request computerCommandRequest
		target  error
	}{
		{name: "missing command", request: computerCommandRequest{}, target: errComputerCommandInvalid},
		{name: "empty executable", request: computerCommandRequest{Command: []string{""}}, target: errComputerCommandInvalid},
		{name: "nul argument", request: computerCommandRequest{Command: []string{"x", "\x00"}}, target: errComputerCommandInvalid},
		{name: "too many arguments", request: computerCommandRequest{Command: tooManyArgs}, target: errComputerCommandTooLarge},
		{name: "cwd escape", request: computerCommandRequest{Command: []string{"x"}, Cwd: "/workspace/../etc"}, target: errComputerCommandInvalid},
		{name: "cwd sibling", request: computerCommandRequest{Command: []string{"x"}, Cwd: "/workspace-other"}, target: errComputerCommandInvalid},
		{name: "reserved env", request: computerCommandRequest{Command: []string{"x"}, Env: map[string]string{"HELMR_TOKEN": "x"}}, target: errComputerCommandInvalid},
		{name: "too many env", request: computerCommandRequest{Command: []string{"x"}, Env: tooManyEnv}, target: errComputerCommandTooLarge},
		{name: "stdin", request: computerCommandRequest{Command: []string{"x"}, Stdin: make([]byte, computerCommandStdinMaxBytes+1)}, target: errComputerCommandStdinTooLarge},
		{name: "timeout", request: computerCommandRequest{Command: []string{"x"}, Timeout: computerCommandMaxTimeout + time.Millisecond}, target: errComputerCommandInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeComputerCommand(test.request); !errors.Is(err, test.target) {
				t.Fatalf("error = %v, want %v", err, test.target)
			}
		})
	}
}

func TestNormalizeIdempotencyKeyRejectsInsteadOfRewriting(t *testing.T) {
	if value, err := normalizeIdempotencyKey("exec:1"); err != nil || value != "exec:1" {
		t.Fatalf("normalized = %q, %v", value, err)
	}
	for _, value := range []string{" exec:1", "exec:1 ", "\x00", string([]byte{0xff})} {
		if _, err := normalizeIdempotencyKey(value); err == nil {
			t.Fatalf("invalid idempotency key %q was accepted", value)
		}
	}
}
