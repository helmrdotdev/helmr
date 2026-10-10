package guestd

import (
	"context"
	"strings"
	"testing"
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

func TestCommandCancellationPreservesPeerAndReplay(t *testing.T) {
	started := make(chan string, 2)
	finishPeer := make(chan struct{})
	entry := &computerMountEntry{basicExecRun: func(ctx context.Context, r *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
		started <- r.Envelope.OperationId
		if r.Envelope.OperationId == "target" {
			<-ctx.Done()
			return computerBasicCommandFailure(r.Envelope.RequestFingerprint, "computer_command_cancelled", ctx.Err())
		}
		select {
		case <-ctx.Done():
			return computerBasicCommandFailure(r.Envelope.RequestFingerprint, "computer_command_cancelled", ctx.Err())
		case <-finishPeer:
		}
		return &computerv0.ComputerBasicExecResult{Outcome: "exited", RequestFingerprint: r.Envelope.RequestFingerprint}
	}}
	registry := testComputerBasicExecRegistry(t, entry)
	target := testComputerBasicExecRequest("target", strings.Repeat("a", 64))
	peer := testComputerBasicExecRequest("peer", strings.Repeat("b", 64))
	execution, _, err := registry.startComputerBasicExec(t.Context(), entry, target)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := registry.startComputerBasicExec(t.Context(), entry, peer)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	<-started
	for range 2 {
		if err := registry.cancelCommand(t.Context(), target.Envelope); err != nil {
			t.Fatal(err)
		}
	}
	<-execution.done
	if execution.result.GetOutcome() != "computer_command_cancelled" {
		t.Fatal(execution.result)
	}
	select {
	case <-other.done:
		t.Fatal("peer cancelled")
	default:
	}
	replay := framedBasicExec(t, t.Context(), registry, target)
	if replay.GetOutcome() != "computer_command_cancelled" {
		t.Fatal(replay)
	}
	close(finishPeer)
	<-other.done
	if other.result.GetOutcome() != "exited" || entry.stopping {
		t.Fatal("peer or Computer stopped")
	}
	if err := registry.cancelCommand(t.Context(), peer.Envelope); err != nil {
		t.Fatal(err)
	}
	if other.result.GetOutcome() != "exited" {
		t.Fatal("late cancellation rewrote completed result")
	}
	for _, stream := range []string{"stdout", "stderr"} {
		commandSpoolBytes(t, entry.commands[target.Envelope.OperationId].output, stream)
	}
	if err := registry.releaseCommand(target.Envelope, false); err != nil {
		t.Fatal(err)
	}
}

func TestCommandCancellationBeforeLaunch(t *testing.T) {
	entry := &computerMountEntry{basicExecRun: func(context.Context, *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
		t.Error("cancelled Command launched")
		return nil
	}}
	registry := testComputerBasicExecRegistry(t, entry)
	request := testComputerBasicExecRequest("target", strings.Repeat("a", 64))
	for _, change := range []func(*computerv0.ComputerCommandAuthority){
		func(a *computerv0.ComputerCommandAuthority) { a.WriterGeneration++ },
		func(a *computerv0.ComputerCommandAuthority) { a.ChannelCredential = "wrong" },
		func(a *computerv0.ComputerCommandAuthority) {
			a.OperationExpiresAtUnixNano = time.Now().Add(-time.Second).UnixNano()
		},
	} {
		wrong := proto.Clone(request.Envelope).(*computerv0.ComputerCommandAuthority)
		change(wrong)
		if err := registry.cancelCommand(t.Context(), wrong); err == nil {
			t.Fatal("invalid cancellation admitted")
		}
	}
	if len(entry.commands) != 0 {
		t.Fatal("invalid cancellation created receipt")
	}
	if err := registry.cancelCommand(t.Context(), request.Envelope); err != nil {
		t.Fatal(err)
	}
	if got := framedBasicExec(t, t.Context(), registry, request); got.GetOutcome() != "computer_command_cancelled" {
		t.Fatal(got)
	}
	wrong := proto.Clone(request.Envelope).(*computerv0.ComputerCommandAuthority)
	wrong.RequestFingerprint = strings.Repeat("b", 64)
	if err := registry.cancelCommand(t.Context(), wrong); err == nil {
		t.Fatal("fingerprint substitution admitted")
	}
}
