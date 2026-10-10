package command

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestCommandCompletionRejectsAmbiguousResult(t *testing.T) {
	code := int32(0)
	valid := CompletionReport{EnvironmentID: uuid.NewV7(), CommandID: uuid.NewV7(), InstanceID: uuid.NewV7(), WriterGeneration: 1, Outcome: "exited", ExitCode: &code, Stdout: OutputBoundary{ThroughSequence: 1, Complete: true}, Stderr: OutputBoundary{ThroughSequence: 1, Complete: true}}
	for name, mutate := range map[string]func(*CompletionReport){
		"exit without code": func(r *CompletionReport) { r.ExitCode = nil },
		"exit with error":   func(r *CompletionReport) { r.Error = []byte(`{}`) },
		"failure with code": func(r *CompletionReport) { r.Outcome = "computer_command_launch_failed" },
		"unknown outcome":   func(r *CompletionReport) { r.ExitCode = nil; r.Outcome = "unknown" },
		"null error": func(r *CompletionReport) {
			r.ExitCode = nil
			r.Outcome = "computer_command_failed"
			r.Error = []byte(`null`)
		},
		"array error": func(r *CompletionReport) {
			r.ExitCode = nil
			r.Outcome = "computer_command_failed"
			r.Error = []byte(`[]`)
		},
		"missing stdout boundary": func(r *CompletionReport) { r.Stdout = OutputBoundary{} },
		"missing stderr boundary": func(r *CompletionReport) { r.Stderr = OutputBoundary{} },
		"zero generation":         func(r *CompletionReport) { r.WriterGeneration = 0 },
		"nil instance":            func(r *CompletionReport) { r.InstanceID = uuid.Nil() },
		"UUIDv4 command":          func(r *CompletionReport) { r.CommandID = uuid.MustParse("8fa3431e-c649-4ea0-bf12-b8e9fcdf1d8d") },
		"UUIDv4 organization":     func(r *CompletionReport) { r.EnvironmentID = uuid.MustParse("8fa3431e-c649-4ea0-bf12-b8e9fcdf1d8d") },
	} {
		t.Run(name, func(t *testing.T) {
			report := valid
			mutate(&report)
			if err := report.Validate(); !errors.Is(err, ErrInvalidCompletion) {
				t.Fatalf("accepted ambiguous result %+v: %v", report, err)
			}
		})
	}
	result, err := valid.parse()
	if err != nil || result.status != "exited" || !result.releaseSafe {
		t.Fatalf("valid result=%+v err=%v", result, err)
	}
}

// The recorded error document is normalized to a JSON object, and a failure
// without one records its outcome as the code.
func TestCommandCompletionNormalizesErrorDocument(t *testing.T) {
	report := CompletionReport{EnvironmentID: uuid.NewV7(), CommandID: uuid.NewV7(), InstanceID: uuid.NewV7(), WriterGeneration: 1, Outcome: "computer_command_failed", Stdout: OutputBoundary{ThroughSequence: 1, Complete: true}, Stderr: OutputBoundary{ThroughSequence: 1, Complete: true}}
	result, err := report.parse()
	if err != nil || string(result.detail) != `{"code":"computer_command_failed"}` {
		t.Fatalf("default detail=%s err=%v", result.detail, err)
	}
	report.Error = []byte(`{"zz":1, "a":{"b":2}}`)
	result, err = report.parse()
	if err != nil || string(result.detail) != `{"a":{"b":2},"zz":1}` {
		t.Fatalf("object detail=%s err=%v", result.detail, err)
	}
}

func TestCommandReleaseLeavesRecoveryScopesUnreconciled(t *testing.T) {
	for _, reason := range []string{"secret_revoked", "instance_lost", "worker_lost"} {
		report, err := release(db.ComputerCommand{Status: "failed", TerminalAt: pgvalue.Timestamptz(time.Now()), TerminalReasonCode: pgvalue.Text(reason)}, uuid.NewV7(), Lease{})
		if err != nil || report != nil {
			t.Fatalf("recovery reason %s blocked peer claims: %v", reason, err)
		}
	}
}
