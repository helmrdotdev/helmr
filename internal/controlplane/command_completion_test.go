package controlplane

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestCommandCompletionRejectsAmbiguousResult(t *testing.T) {
	valid := workerapi.ComputerCommandCompleteRequest{OrgID: uuid.NewV7().String(), CommandID: uuid.NewV7().String(), ComputerInstanceID: uuid.NewV7().String(), WriterGeneration: 1, Outcome: "exited"}
	code := int32(0)
	valid.ExitCode = &code
	for _, mutate := range []func(*workerapi.ComputerCommandCompleteRequest){
		func(r *workerapi.ComputerCommandCompleteRequest) { r.ExitCode = nil },
		func(r *workerapi.ComputerCommandCompleteRequest) { r.Error = []byte(`{}`) },
		func(r *workerapi.ComputerCommandCompleteRequest) { r.Outcome = "computer_command_launch_failed" },
		func(r *workerapi.ComputerCommandCompleteRequest) { r.ExitCode = nil; r.Outcome = "unknown" },
		func(r *workerapi.ComputerCommandCompleteRequest) {
			r.ExitCode = nil
			r.Outcome = "computer_command_failed"
			r.Error = []byte(`null`)
		},
		func(r *workerapi.ComputerCommandCompleteRequest) {
			r.ExitCode = nil
			r.Outcome = "computer_command_failed"
			r.Error = []byte(`[]`)
		},
		func(r *workerapi.ComputerCommandCompleteRequest) { r.WriterGeneration = 0 },
		func(r *workerapi.ComputerCommandCompleteRequest) { r.ComputerInstanceID = " " + r.ComputerInstanceID },
	} {
		request := valid
		mutate(&request)
		if _, err := parseCommandCompletion(request); err == nil {
			t.Fatalf("accepted ambiguous result: %+v", request)
		}
	}
	if result, err := parseCommandCompletion(valid); err != nil || result.status != "exited" || !result.releaseSafe {
		t.Fatalf("valid result=%+v err=%v", result, err)
	}
}

func TestCommandReleaseLeavesRecoveryScopesUnreconciled(t *testing.T) {
	for _, reason := range []string{"secret_revoked", "instance_lost", "worker_lost"} {
		release, err := commandRelease(db.ComputerCommand{Status: "failed", TerminalAt: pgvalue.Timestamptz(time.Now()), TerminalReasonCode: pgvalue.Text(reason)}, "org", "fingerprint")
		if err != nil || release != nil {
			t.Fatalf("recovery reason %s blocked peer claims: %v", reason, err)
		}
	}
}
