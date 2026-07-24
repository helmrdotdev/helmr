package control

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestNormalizeWorkspaceExecAppliesClosedDefaults(t *testing.T) {
	normalized, err := normalizeWorkspaceExec(workspaceExecRequest{
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
	if string(normalized.requestJSON) != `{"command":["printf","","ok"],"cwd":"/workspace","env":{"LANG":"C.UTF-8"},"timeout_ms":300000}` {
		t.Fatalf("request JSON = %s", normalized.requestJSON)
	}
}

func TestNonNilWorkspaceExecBytesClonesInput(t *testing.T) {
	if value := nonNilWorkspaceExecBytes(nil); value == nil || len(value) != 0 {
		t.Fatalf("empty value = %#v", value)
	}
	source := []byte("secret")
	cloned := nonNilWorkspaceExecBytes(source)
	clear(source)
	if string(cloned) != "secret" {
		t.Fatalf("cloned value = %q", cloned)
	}
}

func TestNormalizeWorkspaceExecRejectsInvalidAuthorityAndBounds(t *testing.T) {
	tooManyArgs := make([]string, workspaceExecArgMaxCount+1)
	for index := range tooManyArgs {
		tooManyArgs[index] = "x"
	}
	tooManyEnv := make(map[string]string, workspaceExecEnvMaxCount+1)
	for index := range workspaceExecEnvMaxCount + 1 {
		tooManyEnv["K"+strings.Repeat("X", index)] = "v"
	}
	tests := []struct {
		name    string
		request workspaceExecRequest
		target  error
	}{
		{name: "missing command", request: workspaceExecRequest{}, target: errWorkspaceExecInvalid},
		{name: "empty executable", request: workspaceExecRequest{Command: []string{""}}, target: errWorkspaceExecInvalid},
		{name: "nul argument", request: workspaceExecRequest{Command: []string{"x", "\x00"}}, target: errWorkspaceExecInvalid},
		{name: "too many arguments", request: workspaceExecRequest{Command: tooManyArgs}, target: errWorkspaceExecTooLarge},
		{name: "cwd escape", request: workspaceExecRequest{Command: []string{"x"}, Cwd: "/workspace/../etc"}, target: errWorkspaceExecInvalid},
		{name: "cwd sibling", request: workspaceExecRequest{Command: []string{"x"}, Cwd: "/workspace-other"}, target: errWorkspaceExecInvalid},
		{name: "reserved env", request: workspaceExecRequest{Command: []string{"x"}, Env: map[string]string{"HELMR_TOKEN": "x"}}, target: errWorkspaceExecInvalid},
		{name: "too many env", request: workspaceExecRequest{Command: []string{"x"}, Env: tooManyEnv}, target: errWorkspaceExecTooLarge},
		{name: "stdin", request: workspaceExecRequest{Command: []string{"x"}, Stdin: make([]byte, workspaceExecStdinMaxBytes+1)}, target: errWorkspaceExecStdinTooLarge},
		{name: "timeout", request: workspaceExecRequest{Command: []string{"x"}, Timeout: workspaceExecMaxTimeout + time.Millisecond}, target: errWorkspaceExecInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeWorkspaceExec(test.request); !errors.Is(err, test.target) {
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

func TestNormalizeWorkspaceExecOutcomeCapturesEveryNormalExit(t *testing.T) {
	exitCode := int32(17)
	kind, reason, resultError, err := normalizeWorkspaceExecOutcome(
		api.WorkerWorkspaceExecCompleteRequest{
			Outcome: "exited", ExitCode: &exitCode,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "capture" ||
		reason != "workspace_exec_completed" ||
		resultError != nil {
		t.Fatalf("outcome = %q %q %s", kind, reason, resultError)
	}
}

func TestNormalizeWorkspaceExecOutcomeDiscardsAbnormalExit(t *testing.T) {
	kind, reason, resultError, err := normalizeWorkspaceExecOutcome(
		api.WorkerWorkspaceExecCompleteRequest{
			Outcome: "workspace_exec_timed_out",
			Error:   json.RawMessage(`{"code":"workspace_exec_timed_out"}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "discard" ||
		reason != "workspace_exec_timed_out" ||
		string(resultError) != `{"code":"workspace_exec_timed_out"}` {
		t.Fatalf("outcome = %q %q %s", kind, reason, resultError)
	}
}

func TestNormalizeWorkspaceExecOutcomeAcceptsClosedExecutionFailures(t *testing.T) {
	for _, outcome := range []string{
		"workspace_exec_failed",
		"workspace_exec_secret_delivery_failed",
		"workspace_exec_launch_failed",
		"workspace_exec_output_limit_exceeded",
		"workspace_exec_result_uncertain",
	} {
		t.Run(outcome, func(t *testing.T) {
			kind, reason, _, err := normalizeWorkspaceExecOutcome(
				api.WorkerWorkspaceExecCompleteRequest{
					Outcome: outcome,
					Error:   json.RawMessage(`{"code":"` + outcome + `"}`),
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if kind != "discard" || reason != outcome {
				t.Fatalf("outcome = %q %q", kind, reason)
			}
		})
	}
}

func TestNormalizeWorkspaceExecOutcomeRejectsAmbiguousExit(t *testing.T) {
	if _, _, _, err := normalizeWorkspaceExecOutcome(
		api.WorkerWorkspaceExecCompleteRequest{Outcome: "exited"},
	); err == nil {
		t.Fatal("normal exit without exit_code was accepted")
	}
}

func TestWorkspaceExecPublicationSecretsRequireCurrentResolution(t *testing.T) {
	processID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	valid := db.LockProcessSecretDeliveryRow{
		Secret: db.Secret{
			State:                "active",
			RevocationGeneration: 3,
		},
		ResolutionID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ResolutionProcessID:            processID,
		ResolutionSecretVersionID:      pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ResolutionRevocationGeneration: pgtype.Int8{Int64: 3, Valid: true},
	}
	if !workspaceExecPublicationSecretsValid(
		[]db.LockProcessSecretDeliveryRow{valid},
		processID,
	) {
		t.Fatal("current Secret resolution was rejected")
	}
	revoked := valid
	revoked.Secret.State = "revoked"
	if workspaceExecPublicationSecretsValid(
		[]db.LockProcessSecretDeliveryRow{revoked},
		processID,
	) {
		t.Fatal("revoked Secret resolution was accepted")
	}
	stale := valid
	stale.Secret.RevocationGeneration++
	if workspaceExecPublicationSecretsValid(
		[]db.LockProcessSecretDeliveryRow{stale},
		processID,
	) {
		t.Fatal("stale Secret resolution was accepted")
	}
	missing := valid
	missing.ResolutionID = pgtype.UUID{}
	if workspaceExecPublicationSecretsValid(
		[]db.LockProcessSecretDeliveryRow{missing},
		processID,
	) {
		t.Fatal("missing Secret resolution was accepted")
	}
}
