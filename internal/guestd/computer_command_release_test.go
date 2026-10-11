package guestd

import (
	"context"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestCommandReleaseAllowsCaptureWithoutRelaunch(t *testing.T) {
	var executions atomic.Int32
	entry := &computerMountEntry{basicExecRun: func(_ context.Context, r *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
		executions.Add(1)
		return &computerv0.ComputerBasicExecResult{Outcome: "exited", RequestFingerprint: r.GetEnvelope().GetRequestFingerprint()}
	}}
	registry := testComputerBasicExecRegistry(t, entry)
	request := testComputerBasicExecRequest("command-1", strings.Repeat("a", 64))
	if result := registry.runComputerBasicExec(t.Context(), entry, request); result.GetOutcome() != "exited" {
		t.Fatal(result)
	}
	freeze := &agentv1.ComputerSessionCapture{CheckpointId: "checkpoint", DesiredVersion: 1}
	if _, err := registry.sealAgentComputerCapture(entry, freeze); err == nil {
		t.Fatal("capture accepted unacknowledged result")
	}
	wrong := proto.Clone(request.Envelope).(*computerv0.ComputerCommandAuthority)
	wrong.WriterGeneration++
	if err := registry.releaseCommand(wrong, false); err == nil {
		t.Fatal("wrong writer released result")
	}
	if err := registry.releaseCommand(request.Envelope, false); err == nil {
		t.Fatal("unsettled output released")
	}
	for _, stream := range []string{"stdout", "stderr"} {
		commandSpoolBytes(t, entry.commands["command-1"].output, stream)
	}
	for range 2 {
		if err := registry.releaseCommand(request.Envelope, false); err != nil {
			t.Fatal(err)
		}
	}
	if !entry.commands["command-1"].output.closed {
		t.Fatal("output buffers retained")
	}
	if result := registry.runComputerBasicExec(t.Context(), entry, request); result.GetOutcome() == "exited" {
		t.Fatal("released Command relaunched")
	}
	if executions.Load() != 1 {
		t.Fatal("duplicate execution")
	}
	if _, err := registry.sealAgentComputerCapture(entry, freeze); err != nil {
		t.Fatal(err)
	}
}

func TestCommandReleaseRejectsRunningOrUncertainScope(t *testing.T) {
	for _, outcome := range []string{"computer_command_scope_termination_failed", "computer_command_result_uncertain"} {
		t.Run(outcome, func(t *testing.T) {
			entry := &computerMountEntry{basicExecRun: func(_ context.Context, r *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
				return &computerv0.ComputerBasicExecResult{Outcome: outcome, RequestFingerprint: r.GetEnvelope().GetRequestFingerprint()}
			}}
			registry := testComputerBasicExecRegistry(t, entry)
			request := testComputerBasicExecRequest("command", strings.Repeat("b", 64))
			registry.runComputerBasicExec(t.Context(), entry, request)
			if err := registry.releaseCommand(request.Envelope, false); err == nil {
				t.Fatal("uncertain scope released")
			}
			if !entry.hasUnreleasedCommands() {
				t.Fatal("capture barrier cleared")
			}
		})
	}
}
