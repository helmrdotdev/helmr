package computerhost

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

func sessionTransportGrant() *agentv1.SessionGrant {
	return &agentv1.SessionGrant{Identity: &agentv1.SessionIdentity{SessionId: "session", ProcessEpoch: 1}, ComputerId: "computer", ComputerInstanceId: "instance", WriterGeneration: 1, WorkerHostId: "host", ComputerLeaseEpoch: 2, AuthorityGeneration: 3, ExpiresAtUnixNano: time.Now().Add(time.Hour).UnixNano(), ChannelCredential: "guest-private"}
}
func sessionTransportMachine(t *testing.T, handle func(net.Conn, *agentv1.SessionAttach)) *agentControlMachine {
	t.Helper()
	return &agentControlMachine{handle: func(stream net.Conn) {
		header, _, err := wire.ReadStreamFrameHeader(stream)
		if err != nil {
			t.Error(err)
			return
		}
		if header.Type != wire.StreamTypeAgentSession {
			t.Errorf("wrong stream %v", header)
			return
		}
		var request agentv1.SessionAttach
		if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &request); err != nil {
			t.Error(err)
			return
		}
		attached := &agentv1.GuestSessionMessage{Identity: request.GetGrant().GetIdentity(), AttachmentSequence: request.GetAttachmentSequence(), Message: &agentv1.GuestSessionMessage_Attached{Attached: &agentv1.SessionAttached{Ready: true}}}
		if err := frameio.WriteProtoFrame(stream, attached); err != nil {
			t.Error(err)
			return
		}
		handle(stream, &request)
	}}
}
func TestAgentSessionTransportPreservesOriginalOperationAuthority(t *testing.T) {
	grant := sessionTransportGrant()
	machine := sessionTransportMachine(t, func(stream net.Conn, request *agentv1.SessionAttach) {
		event := &agentv1.ProgramEvent{Identity: grant.Identity, Event: &agentv1.ProgramEvent_Operation{Operation: &agentv1.Operation{RequestId: "original", Method: agentv1.Operation_METHOD_RUNTIME_MCP, PayloadJson: []byte(`{"tool":"enqueue","arguments":{"idempotencyKey":"original"}}`)}}}
		message := &agentv1.GuestSessionMessage{Identity: grant.Identity, AttachmentSequence: request.AttachmentSequence, EventSequence: 7, OperationAuthorityGeneration: 2, OperationLeaseEpoch: 2, Message: &agentv1.GuestSessionMessage_Event{Event: event}}
		if err := frameio.WriteProtoFrame(stream, message); err != nil {
			t.Error(err)
			return
		}
		var ack agentv1.HostSessionMessage
		if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &ack); err != nil {
			t.Error(err)
			return
		}
		if ack.GetAcknowledged().GetThroughSequence() != 7 || ack.GetAttachmentSequence() != 4 {
			t.Error("ack did not bind exact attachment")
		}
	})
	connection, attached, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 4})
	if err != nil || !attached.GetReady() {
		t.Fatalf("attach %v %v", attached, err)
	}
	defer connection.Close()
	event, err := connection.Receive(t.Context())
	if err != nil || event.GetOperationAuthorityGeneration() != 2 || event.GetEvent().GetOperation().GetRequestId() != "original" {
		t.Fatalf("event changed: %v %v", event, err)
	}
	if err := connection.Acknowledge(t.Context(), 8); err == nil {
		t.Fatal("acknowledged an unreceived event")
	}
	if err := connection.Acknowledge(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
}
func TestAgentSessionTransportRenewsWithoutSetupAfterTransientFailure(t *testing.T) {
	grant := sessionTransportGrant()
	grant.ExpiresAtUnixNano = time.Now().Add(60 * time.Millisecond).UnixNano()
	observed := make(chan *agentv1.SessionGrant, 1)
	machine := sessionTransportMachine(t, func(stream net.Conn, request *agentv1.SessionAttach) {
		if request.GetStart() != nil {
			t.Error("reattachment tried setup")
		}
		var message agentv1.HostSessionMessage
		if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &message); err != nil {
			t.Error(err)
			return
		}
		observed <- message.GetRenew()
		_, _ = io.Copy(io.Discard, stream)
	})
	connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- connection.MaintainAuthority(ctx, func(_ context.Context, current *agentv1.SessionGrant) (*agentv1.SessionGrant, error) {
			if calls.Add(1) == 1 {
				return nil, errors.New("temporary CP outage")
			}
			next := proto.Clone(current).(*agentv1.SessionGrant)
			next.ExpiresAtUnixNano = time.Now().Add(time.Hour).UnixNano()
			next.AuthorityGeneration++
			return next, nil
		})
	}()
	select {
	case renewed := <-observed:
		if renewed.GetAuthorityGeneration() != 4 || !proto.Equal(renewed.GetIdentity(), grant.Identity) {
			t.Fatal("renewed another identity")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("host did not retry authority renewal")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestAgentSessionTransportReadCancellationClosesOnlyAttachment(t *testing.T) {
	closed := make(chan struct{})
	machine := sessionTransportMachine(t, func(stream net.Conn, _ *agentv1.SessionAttach) { _, _ = io.Copy(io.Discard, stream); close(closed) })
	connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: sessionTransportGrant(), AttachmentSequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := connection.Receive(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("read cancellation did not join stream close")
	}
}
func TestAgentSessionTransportRejectsForgedEnvelope(t *testing.T) {
	for _, variant := range []string{"identity", "attachment", "generation", "sequence"} {
		t.Run(variant, func(t *testing.T) {
			grant := sessionTransportGrant()
			machine := sessionTransportMachine(t, func(stream net.Conn, _ *agentv1.SessionAttach) {
				message := &agentv1.GuestSessionMessage{Identity: proto.Clone(grant.Identity).(*agentv1.SessionIdentity), AttachmentSequence: 1, EventSequence: 1, OperationAuthorityGeneration: 3, OperationLeaseEpoch: 2, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: grant.Identity, Event: &agentv1.ProgramEvent_Operation{Operation: &agentv1.Operation{RequestId: "request", Method: agentv1.Operation_METHOD_RUNTIME_MCP}}}}}
				switch variant {
				case "identity":
					message.Identity.SessionId = "other"
				case "attachment":
					message.AttachmentSequence = 2
				case "generation":
					message.OperationAuthorityGeneration = 0
				case "sequence":
					message.EventSequence = 0
				}
				_ = frameio.WriteProtoFrame(stream, message)
			})
			connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if _, err := connection.Receive(t.Context()); err == nil {
				t.Fatal("forged event accepted")
			}
		})
	}
}

func TestAgentSessionRenewalPropagatesGenerationAtLeaseExpiryCap(t *testing.T) {
	grant := sessionTransportGrant()
	grant.ExpiresAtUnixNano = time.Now().Add(100 * time.Millisecond).UnixNano()
	observed := make(chan *agentv1.SessionGrant, 1)
	machine := sessionTransportMachine(t, func(stream net.Conn, _ *agentv1.SessionAttach) {
		var message agentv1.HostSessionMessage
		if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &message); err != nil {
			return
		}
		observed <- message.GetRenew()
		_, _ = io.Copy(io.Discard, stream)
	})
	connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- connection.MaintainAuthority(ctx, func(context.Context, *agentv1.SessionGrant) (*agentv1.SessionGrant, error) {
			next := proto.Clone(grant).(*agentv1.SessionGrant)
			next.AuthorityGeneration++
			return next, nil
		})
	}()
	select {
	case next := <-observed:
		if next.AuthorityGeneration != grant.AuthorityGeneration+1 || next.ExpiresAtUnixNano != grant.ExpiresAtUnixNano {
			t.Fatal("generation or expiry changed incorrectly")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("generation did not propagate at fixed lease expiry")
	}
	cancel()
	_ = connection.Close()
	<-done
}

func TestAgentSessionPeriodicRenewalOvertakenByControl(t *testing.T) {
	grant := sessionTransportGrant()
	grant.ExpiresAtUnixNano = time.Now().Add(100 * time.Millisecond).UnixNano()
	machine := sessionTransportMachine(t, func(stream net.Conn, _ *agentv1.SessionAttach) { _, _ = io.Copy(io.Discard, stream) })
	connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	response := make(chan struct{})
	requested := make(chan struct{})
	continued := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		calls := 0
		done <- connection.MaintainAuthority(ctx, func(ctx context.Context, current *agentv1.SessionGrant) (*agentv1.SessionGrant, error) {
			calls++
			if calls == 1 {
				close(requested)
				select {
				case <-response:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				current.ExpiresAtUnixNano += int64(time.Second)
				return current, nil
			}
			close(continued)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}()
	select {
	case <-requested:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	next := proto.Clone(grant).(*agentv1.SessionGrant)
	next.AuthorityGeneration++
	next.ExpiresAtUnixNano += int64(2 * time.Second)
	if err := connection.Renew(ctx, next); err != nil {
		t.Fatal(err)
	}
	close(response)
	select {
	case <-continued:
	case err := <-done:
		t.Fatalf("overtaken response killed attachment: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !proto.Equal(connection.CurrentGrant(), next) {
		t.Fatal("overtaken response changed newer grant")
	}
	changedOwner := proto.Clone(grant).(*agentv1.SessionGrant)
	changedOwner.WorkerHostId = "another-host"
	if err := connection.reconcileRenewal(ctx, changedOwner); err == nil {
		t.Fatal("obsolete response changed physical owner")
	}
	cancel()
	<-done
}
