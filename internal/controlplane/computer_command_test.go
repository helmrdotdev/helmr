package controlplane

import (
	"net/http"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCommandOutcomeSeparatesFailureExitAndReconciliation(t *testing.T) {
	base := db.ComputerCommand{ID: pgvalue.NewUUIDv7(), ComputerID: pgvalue.NewUUIDv7(), ComputerLeaseEpoch: pgtype.Int8{Int64: 1, Valid: true}, TerminalAt: pgvalue.Timestamptz(time.Now()), Status: db.ComputerCommandStatusFailed, ExitCode: pgtype.Int4{Int32: 0, Valid: true}, FailureReason: pgvalue.Text("scope_termination_failed")}
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

func TestPreparationExhaustedCommandIsDispatchFailure(t *testing.T) {
	info, err := publicCommandInfo(db.ComputerCommand{ID: pgvalue.NewUUIDv7(), ComputerID: pgvalue.NewUUIDv7(), Status: db.ComputerCommandStatusFailed, TerminalAt: pgvalue.Timestamptz(time.Now()), TerminalReasonCode: pgvalue.Text("computer_preparation_exhausted"), FailureReason: pgvalue.Text("dispatch_failed")})
	if err != nil {
		t.Fatal(err)
	}
	if info.Outcome == nil || info.Outcome.Failure == nil || info.Outcome.Failure.Reason != "dispatch_failed" || info.Outcome.ExitCode != nil || !info.ProcessReconciled {
		t.Fatalf("outcome=%+v", info)
	}
}

// The public projection reports every Command state, and the outcome of a
// terminal one.
func TestPublicCommandInfoProjectsEveryState(t *testing.T) {
	for _, test := range []struct {
		name    string
		command db.ComputerCommand
	}{
		{"pending", db.ComputerCommand{Status: db.ComputerCommandStatusPending}},
		{"starting", db.ComputerCommand{Status: db.ComputerCommandStatusStarting}},
		{"running", db.ComputerCommand{Status: db.ComputerCommandStatusRunning}},
		{"stopping", db.ComputerCommand{Status: db.ComputerCommandStatusStopping}},
		{"exited", db.ComputerCommand{Status: db.ComputerCommandStatusExited, ExitCode: pgtype.Int4{Int32: 17, Valid: true}, TerminalAt: pgvalue.Timestamptz(time.Now())}},
		{"timed_out", db.ComputerCommand{Status: db.ComputerCommandStatusTimedOut, TerminalAt: pgvalue.Timestamptz(time.Now())}},
		{"cancelled", db.ComputerCommand{Status: db.ComputerCommandStatusCancelled, TerminalAt: pgvalue.Timestamptz(time.Now())}},
		{"lost", db.ComputerCommand{Status: db.ComputerCommandStatusLost, FailureReason: pgvalue.Text("guest_failure"), TerminalAt: pgvalue.Timestamptz(time.Now())}},
		{"failed", db.ComputerCommand{Status: db.ComputerCommandStatusFailed, FailureReason: pgvalue.Text("dispatch_failed"), TerminalAt: pgvalue.Timestamptz(time.Now())}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.command.ID, test.command.ComputerID = pgvalue.NewUUIDv7(), pgvalue.NewUUIDv7()
			info, err := publicCommandInfo(test.command)
			if err != nil || info.ID != pgvalue.UUIDString(test.command.ID) || info.ComputerID != pgvalue.UUIDString(test.command.ComputerID) || info.Status != test.name {
				t.Fatalf("info = %+v, %v", info, err)
			}
			if terminal := test.command.TerminalAt.Valid; terminal != (info.Outcome != nil) {
				t.Fatalf("outcome = %+v", info.Outcome)
			}
			if test.name == "exited" && (info.Outcome.Kind != "exited" || *info.Outcome.ExitCode != 17) {
				t.Fatalf("exited outcome = %+v", info.Outcome)
			}
			if test.name == "failed" && (info.Outcome.Failure == nil || info.Outcome.Failure.Reason != "dispatch_failed") {
				t.Fatalf("failed outcome = %+v", info.Outcome)
			}
		})
	}
	if _, err := publicCommandInfo(db.ComputerCommand{ID: pgvalue.NewUUIDv7(), Status: db.ComputerCommandStatusFailed, ResultPrunedAt: pgvalue.Timestamptz(time.Now())}); errorStatus(err) != http.StatusGone {
		t.Fatalf("pruned result = %v", err)
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
