package command

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestNormalizeAppliesClosedDefaults(t *testing.T) {
	normalized, err := normalize(CreateRequest{
		Argv: []string{"printf", "", "ok"},
		Env:  map[string]string{"LANG": "C.UTF-8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.cwd != "/workspace" ||
		normalized.timeoutMS != 300000 ||
		len(normalized.argv) != 3 ||
		normalized.argv[1] != "" ||
		normalized.stdin == nil ||
		string(normalized.envJSON) != `{"LANG":"C.UTF-8"}` {
		t.Fatalf("normalized = %+v", normalized)
	}
}

func TestNonNilBytesClonesInput(t *testing.T) {
	if value := nonNilBytes(nil); value == nil || len(value) != 0 {
		t.Fatalf("empty value = %#v", value)
	}
	source := []byte("secret")
	cloned := nonNilBytes(source)
	clear(source)
	if string(cloned) != "secret" {
		t.Fatalf("cloned value = %q", cloned)
	}
}

func TestNormalizeRejectsInvalidAuthorityAndBounds(t *testing.T) {
	tooManyArgs := make([]string, argMaxCount+1)
	for index := range tooManyArgs {
		tooManyArgs[index] = "x"
	}
	tooManyEnv := make(map[string]string, envMaxCount+1)
	for index := range envMaxCount + 1 {
		tooManyEnv["K"+strings.Repeat("X", index)] = "v"
	}
	for _, test := range []struct {
		name    string
		request CreateRequest
		kind    InputKind
		message string
	}{
		{"missing command", CreateRequest{}, InputInvalid, "computer exec request is invalid: command is required"},
		{"empty executable", CreateRequest{Argv: []string{""}}, InputInvalid, "computer exec request is invalid: command executable is required"},
		{"nul argument", CreateRequest{Argv: []string{"x", "\x00"}}, InputInvalid, "computer exec request is invalid: command arguments must be valid UTF-8 without NUL"},
		{"too many arguments", CreateRequest{Argv: tooManyArgs}, InputTooLarge, "computer exec request is too large: command has more than 128 arguments"},
		{"cwd escape", CreateRequest{Argv: []string{"x"}, Cwd: "/workspace/../etc"}, InputInvalid, "computer exec request is invalid: cwd must be a canonical absolute path beneath /workspace"},
		{"cwd sibling", CreateRequest{Argv: []string{"x"}, Cwd: "/workspace-other"}, InputInvalid, "computer exec request is invalid: cwd must be a canonical absolute path beneath /workspace"},
		{"reserved env", CreateRequest{Argv: []string{"x"}, Env: map[string]string{"HELMR_TOKEN": "x"}}, InputInvalid, `computer exec request is invalid: env name "HELMR_TOKEN" is invalid or reserved`},
		{"too many env", CreateRequest{Argv: []string{"x"}, Env: tooManyEnv}, InputTooLarge, "computer exec request is too large: env has more than 128 entries"},
		{"stdin", CreateRequest{Argv: []string{"x"}, Stdin: make([]byte, MaxStdinBytes+1)}, InputStdinTooLarge, "computer exec stdin is too large"},
		{"timeout", CreateRequest{Argv: []string{"x"}, Timeout: MaxTimeout + time.Millisecond}, InputInvalid, "computer exec request is invalid: timeout must be between 1ms and 15m"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := normalize(test.request)
			var input InputError
			if !errors.As(err, &input) || input.Kind != test.kind || err.Error() != test.message {
				t.Fatalf("error = %v, want %d %q", err, test.kind, test.message)
			}
		})
	}
}

// The creation receipt is the stored idempotency receipt a replay reads; its
// encoding is pinned.
func TestCreateReceiptEncoding(t *testing.T) {
	encoded, err := json.Marshal(createReceipt{CommandID: "0190b5c2-0000-7000-8000-000000000001"})
	if err != nil || string(encoded) != `{"command_id":"0190b5c2-0000-7000-8000-000000000001"}` {
		t.Fatalf("receipt = %s, %v", encoded, err)
	}
}

// A Computer that failed a capture or recovery, or is in recovery, reports
// recovery before deletion; any other state that does not admit, including
// lost dirty state, is busy, and an admitting Computer that reached its
// preparation limit is exhausted.
func TestAdmitsReportsComputerState(t *testing.T) {
	orgID, projectID := uuid.NewV7(), uuid.NewV7()
	request := CreateRequest{OrgID: orgID, ProjectID: projectID}
	admitting := db.LockComputerAdmissionAuthorityRow{
		OrgID: pgvalue.UUID(orgID), ProjectID: pgvalue.UUID(projectID), Status: db.ComputerStatusActive,
		DesiredState: db.ComputerDesiredStateStopped, DirtyState: db.ComputerDirtyStateClean, HeadDiskVersionID: pgvalue.UUID(uuid.NewV7()),
	}
	for name, test := range map[string]struct {
		mutate func(*db.LockComputerAdmissionAuthorityRow)
		want   error
	}{
		"admits":          {func(*db.LockComputerAdmissionAuthorityRow) {}, nil},
		"capture failed":  {func(r *db.LockComputerAdmissionAuthorityRow) { r.DirtyState = db.ComputerDirtyStateCaptureFailed }, computer.ErrRecoveryRequired},
		"recovery failed": {func(r *db.LockComputerAdmissionAuthorityRow) { r.RecoveryFailure = []byte(`{}`) }, computer.ErrRecoveryRequired},
		"recovery over delete": {func(r *db.LockComputerAdmissionAuthorityRow) {
			r.Status, r.RecoveryFailure = db.ComputerStatusDeleting, []byte(`{}`)
		}, computer.ErrRecoveryRequired},
		"recovery required":     {func(r *db.LockComputerAdmissionAuthorityRow) { r.Status = db.ComputerStatusRecoveryRequired }, computer.ErrRecoveryRequired},
		"deleting":              {func(r *db.LockComputerAdmissionAuthorityRow) { r.Status = db.ComputerStatusDeleting }, computer.ErrDeleting},
		"dirty state lost":      {func(r *db.LockComputerAdmissionAuthorityRow) { r.DirtyState = db.ComputerDirtyStateDirtyStateLost }, computer.ErrBusy},
		"other project":         {func(r *db.LockComputerAdmissionAuthorityRow) { r.ProjectID = pgvalue.UUID(uuid.NewV7()) }, computer.ErrBusy},
		"preparation exhausted": {func(r *db.LockComputerAdmissionAuthorityRow) { r.PreparationFailure = []byte(`{}`) }, computer.ErrPreparationExhausted},
	} {
		t.Run(name, func(t *testing.T) {
			row := admitting
			test.mutate(&row)
			if err := admits(row, request); !errors.Is(err, test.want) {
				t.Fatalf("admits = %v, want %v", err, test.want)
			}
		})
	}
}

// The cancellation receipt is the stored idempotency receipt a replay reads;
// its encoding is pinned.
func TestCancelReceiptEncoding(t *testing.T) {
	encoded, err := json.Marshal(CancelReceipt{ID: "0190b5c2-0000-7000-8000-000000000002", TargetID: "0190b5c2-0000-7000-8000-000000000001", Status: "accepted"})
	if err != nil || string(encoded) != `{"id":"0190b5c2-0000-7000-8000-000000000002","target_id":"0190b5c2-0000-7000-8000-000000000001","status":"accepted"}` {
		t.Fatalf("receipt = %s, %v", encoded, err)
	}
}
