package guestd

import (
	"bytes"
	"context"
	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
	"time"
)

func framedBasicExec(t *testing.T, ctx context.Context, registry *computerOperationRegistry, request *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
	t.Helper()
	var stream bytes.Buffer
	if err := frameio.WriteProtoFrame(&stream, request); err != nil {
		t.Fatal(err)
	}
	if err := handleComputerBasicExecConnection(ctx, &stream, registry); err != nil {
		t.Fatal(err)
	}
	for {
		var event computerv0.ComputerBasicExecEvent
		if err := frameio.ReadProtoFrame(&stream, &event); err != nil {
			t.Fatal(err)
		}
		if result := event.GetResult(); result != nil {
			return result
		}
	}
}

func TestComputerCommandReplayBindsPhysicalAuthority(t *testing.T) {
	entry := &computerMountEntry{basicExecRun: func(_ context.Context, r *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
		return &computerv0.ComputerBasicExecResult{Outcome: "exited", RequestFingerprint: r.GetEnvelope().GetRequestFingerprint()}
	}}
	registry := testComputerBasicExecRegistry(t, entry)
	request := testComputerBasicExecRequest("one", strings.Repeat("a", 64))
	if got := framedBasicExec(t, t.Context(), registry, request); got.GetOutcome() != "exited" {
		t.Fatal(got)
	}
	for _, change := range []func(*computerv0.ComputerBasicExecRequest){func(r *computerv0.ComputerBasicExecRequest) { r.Envelope.ComputerInstanceId = "other" }, func(r *computerv0.ComputerBasicExecRequest) { r.Envelope.WriterGeneration++ }, func(r *computerv0.ComputerBasicExecRequest) { r.Envelope.ChannelToken = "other" }} {
		altered := proto.Clone(request).(*computerv0.ComputerBasicExecRequest)
		change(altered)
		if got := framedBasicExec(t, t.Context(), registry, altered); got.GetOutcome() != "computer_command_fenced" {
			t.Fatal(got)
		}
	}
	request.Envelope.OperationExpiresAtUnixNano = time.Now().Add(2 * time.Minute).UnixNano()
	if got := framedBasicExec(t, t.Context(), registry, request); got.GetOutcome() != "exited" {
		t.Fatal(got)
	}
}

func TestCommandPreservesActiveProgramAuthority(t *testing.T) {
	entry, registry, authority := testProgramMount(t)
	entry.basicExecRun = func(_ context.Context, r *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
		return &computerv0.ComputerBasicExecResult{Outcome: "exited", RequestFingerprint: r.GetEnvelope().GetRequestFingerprint()}
	}
	release, err := registry.admitProgram(entry, authority, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	request := testComputerBasicExecRequest("command-1", strings.Repeat("a", 64))
	request.Envelope.ChannelToken = authority.GetChannelToken()
	request.Envelope.ComputerInstanceId = authority.GetFence().GetComputerInstanceId()
	request.Envelope.WriterGeneration = authority.GetFence().GetWriterGeneration()
	if result := framedBasicExec(t, t.Context(), registry, request); result.GetOutcome() != "exited" {
		t.Fatal(result)
	}
	if !proto.Equal(registry.programClaims[0].authority, authority) || len(registry.programClaims) != 1 {
		t.Fatal("Command replaced active Program authority")
	}
}
