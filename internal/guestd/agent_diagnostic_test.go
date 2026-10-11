package guestd

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

func TestAgentDiagnosticsDrainReplayAndControlIndependence(t *testing.T) {
	session, _ := agentSessionFixture(t)
	session.logs[0], _ = newDiagnosticBuffer(diagnosticLimits{ChunkBytes: 3, BufferBytes: 3, BufferRecords: 1})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	relay := newAgentRelay(ctx, session, nil)
	// Saturate before attachment. The authored writer must finish without any sink.
	input := []byte{0, 255, 128, 1, 2, 3, 4, 5, 6}
	drained := make(chan struct{})
	reader, writer := io.Pipe()
	go func() { defer close(drained); relay.logLoop(reader, agentv1.SessionLog_STREAM_STDOUT) }()
	go func() { _, _ = writer.Write(input); _ = writer.Close() }()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("diagnostic saturation blocked authored output")
	}
	go relay.sendLoop()
	first := attachAgentRelay(t, relay, 1, session.grant)
	read := func(r io.Reader) *agentv1.GuestSessionMessage {
		t.Helper()
		message := new(agentv1.GuestSessionMessage)
		if err := frameio.ReadProtoFrameBounded(r, maxAgentTransportFrameBytes, message); err != nil {
			t.Fatal(err)
		}
		return message
	}
	original := read(first)
	if original.GetEventSequence() != 0 || !bytes.Equal(original.GetLog().GetData(), input[:3]) {
		t.Fatalf("log envelope %v", original)
	}
	// A lifecycle ACK cannot release a diagnostic with the same numeric sequence.
	control := relay.enqueue(&agentv1.GuestSessionMessage{Identity: session.grant.Identity, Message: &agentv1.GuestSessionMessage_Stopped{Stopped: &agentv1.SessionStopped{}}})
	if event := read(first); event.GetStopped() == nil || event.GetEventSequence() != control {
		t.Fatalf("blocked control %v", event)
	}
	if err := writeAgentTransportFrame(first, &agentv1.HostSessionMessage{AttachmentSequence: 1, Message: &agentv1.HostSessionMessage_Acknowledged{Acknowledged: &agentv1.SessionEventsAcknowledged{ThroughSequence: control}}}); err != nil {
		t.Fatal(err)
	}
	waitAgentRelay(t, relay, func() bool { return relay.acknowledged == control })
	_ = first.Close()
	second := attachAgentRelay(t, relay, 2, session.grant)
	replay := read(second)
	if !proto.Equal(original.GetLog(), replay.GetLog()) {
		t.Fatal("uncertain log identity or bytes changed")
	}
	ack := func(message *agentv1.GuestSessionMessage) {
		t.Helper()
		if err := writeAgentTransportFrame(second, &agentv1.HostSessionMessage{AttachmentSequence: 2, Message: &agentv1.HostSessionMessage_LogAcknowledged{LogAcknowledged: &agentv1.SessionLogAcknowledged{Stream: message.GetLog().GetStream(), ThroughSequence: message.GetLog().GetThroughSequence()}}}); err != nil {
			t.Fatal(err)
		}
	}
	ack(replay)
	gap := read(second)
	if gap.GetLog().GetKind() != agentv1.SessionLog_KIND_GAP || gap.GetLog().GetSequence() != 2 || gap.GetLog().GetThroughSequence() != 3 || gap.GetLog().GetDroppedBytes() != 6 {
		t.Fatalf("gap %v", gap)
	}
	ack(gap)
	end := read(second)
	if end.GetLog().GetKind() != agentv1.SessionLog_KIND_END || end.GetLog().GetSequence() != 4 || !end.GetLog().GetComplete() {
		t.Fatalf("EOF %v", end)
	}
	ack(end)
	waitAgentRelay(t, relay, func() bool { _, ok := session.logs[0].peek(); return !ok })
}

func TestTerminalDiagnosticTailDoesNotAdvanceDuringCapture(t *testing.T) {
	session, _ := agentSessionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	relay := newAgentRelay(ctx, session, nil)
	reader, writer := io.Pipe()
	drained := make(chan struct{})
	go func() { defer close(drained); relay.logLoop(reader, agentv1.SessionLog_STREAM_STDOUT) }()
	if _, err := writer.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	<-drained
	session.mu.Lock()
	session.terminal = true
	session.mu.Unlock()
	relay.mu.Lock()
	relay.captureFrozen = true
	relay.mu.Unlock()
	go relay.sendLoop()
	connection := attachAgentRelay(t, relay, 1, session.grant)
	defer connection.Close()
	received := make(chan *agentv1.GuestSessionMessage, 1)
	readErr := make(chan error, 1)
	go func() {
		message := new(agentv1.GuestSessionMessage)
		err := frameio.ReadProtoFrameBounded(connection, maxAgentTransportFrameBytes, message)
		if err != nil {
			readErr <- err
			return
		}
		received <- message
	}()
	select {
	case message := <-received:
		t.Fatalf("terminal tail advanced during capture: %v", message)
	case err := <-readErr:
		t.Fatal(err)
	case <-time.After(50 * time.Millisecond):
	}
	relay.mu.Lock()
	relay.captureFrozen = false
	relay.notifyLocked()
	relay.mu.Unlock()
	select {
	case message := <-received:
		if !bytes.Equal(message.GetLog().GetData(), []byte("tail")) {
			t.Fatalf("retained head changed: %v", message)
		}
	case err := <-readErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("tail did not resume after capture gate opened")
	}
}
