package guestd

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func framedBasicExec(t *testing.T, ctx context.Context, registry *computerOperationRegistry, request *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
	t.Helper()
	host, guest := net.Pipe()
	defer host.Close()
	_ = host.SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan error, 1)
	go func() { defer guest.Close(); done <- handleComputerBasicExecConnection(ctx, guest, registry) }()
	if err := frameio.WriteProtoFrame(host, request); err != nil {
		t.Fatal(err)
	}
	var result *computerv0.ComputerBasicExecResult
	for {
		var event computerv0.ComputerBasicExecEvent
		if err := frameio.ReadProtoFrame(host, &event); err != nil {
			if err != io.EOF || result == nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			return result
		}
		if r := event.GetResult(); r != nil {
			result = r
		}
		if chunk := event.GetOutput(); chunk != nil {
			ackCommand(t, host, chunk)
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
	for _, change := range []func(*computerv0.ComputerBasicExecRequest){func(r *computerv0.ComputerBasicExecRequest) { r.Envelope.ComputerInstanceId = "other" }, func(r *computerv0.ComputerBasicExecRequest) { r.Envelope.WriterGeneration++ }, func(r *computerv0.ComputerBasicExecRequest) { r.Envelope.ChannelCredential = "other" }} {
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

func TestCommandPreservesActiveSessionAuthority(t *testing.T) {
	t.Setenv("HELMR_GUESTD_TMPDIR", t.TempDir())
	registry, capture, _ := agentComputerFixture(t)
	entry := registry.entries[capture.Envelope.ComputerInstanceId]
	entry.basicExecRun = func(_ context.Context, r *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
		return &computerv0.ComputerBasicExecResult{Outcome: "exited", RequestFingerprint: r.GetEnvelope().GetRequestFingerprint()}
	}
	grants := make(map[string]*agentv1.SessionGrant)
	for id, relay := range registry.agentSessions {
		relay.session.activeTurn = "active-turn"
		grants[id] = proto.Clone(relay.session.grant).(*agentv1.SessionGrant)
	}
	request := testComputerBasicExecRequest("command-1", strings.Repeat("a", 64))
	request.Envelope.ComputerId = entry.computerID
	request.Envelope.ChannelCredential = entry.channelCredential
	request.Envelope.ComputerInstanceId = entry.computerInstanceID
	request.Envelope.WriterGeneration = int64(entry.currentWriterGeneration())
	if result := framedBasicExec(t, t.Context(), registry, request); result.GetOutcome() != "exited" {
		t.Fatal(result)
	}
	if len(registry.agentSessions) != len(grants) {
		t.Fatal("Command changed Session membership")
	}
	for id, grant := range grants {
		relay := registry.agentSessions[id]
		if relay == nil || !proto.Equal(relay.session.grant, grant) || relay.session.activeTurn != "active-turn" {
			t.Fatal("Command changed active Session authority or Turn")
		}
	}
}
