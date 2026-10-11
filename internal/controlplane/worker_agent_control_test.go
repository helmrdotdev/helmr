package controlplane

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/computerhost"
	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerAgentDurableControlsReachRetainedProcess(t *testing.T) {
	f := agenttest.New(t)
	handler := newPostgresServer(t, f.Pool)
	var prepares atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/sessions/control" && prepares.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	attachment, err := client.AcquireAgentAttachment(t.Context(), runtimeTestSession(f))
	if err != nil {
		t.Fatal(err)
	}
	identity := &agentv1.SessionIdentity{SessionId: f.Session.String(), ProcessEpoch: 1}
	finished := make(chan error, 1)
	machine := &workerAgentTestMachine{handle: func(stream net.Conn) {
		run := func() error {
			if _, _, err := wire.ReadStreamFrameHeader(stream); err != nil {
				return err
			}
			var attach agentv1.SessionAttach
			if err := frameio.ReadProtoFrameBounded(stream, 1<<20, &attach); err != nil {
				return err
			}
			if attach.GetStart() != nil {
				return errors.New("control attachment reran setup")
			}
			if err := frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: identity, AttachmentSequence: attach.AttachmentSequence, Message: &agentv1.GuestSessionMessage_Attached{Attached: &agentv1.SessionAttached{Ready: true}}}); err != nil {
				return err
			}
			var hold agent.SessionControlReceipt
			for index, kind := range []string{"resume", "suspend", "resume", "shutdown"} {
				var command *agentv1.GuestCommand
				for command == nil {
					var message agentv1.HostSessionMessage
					if err := frameio.ReadProtoFrameBounded(stream, 1<<20, &message); err != nil {
						return err
					}
					command = message.GetCommand()
				}
				if command.ControlSequence != int64(index+1) || (kind == "resume" && command.GetResume() == nil) || (kind == "suspend" && command.GetSuspend() == nil) || (kind == "shutdown" && command.GetShutdown() == nil) {
					return errors.New("wrong durable control")
				}
				result := &agentv1.DeliveryResult{DeliveryId: command.DeliveryId, Outcome: &agentv1.DeliveryResult_ValueJson{ValueJson: []byte(`null`)}}
				seq := uint64(index + 1)
				if err := frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: identity, AttachmentSequence: attach.AttachmentSequence, EventSequence: seq, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: identity, Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: result}}}}); err != nil {
					return err
				}
				for {
					var ack agentv1.HostSessionMessage
					if err := frameio.ReadProtoFrameBounded(stream, 1<<20, &ack); err != nil {
						return err
					}
					if ack.GetAcknowledged().GetThroughSequence() == seq {
						break
					}
				}
				var acknowledged bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT control_acknowledged_at IS NOT NULL FROM session_processes WHERE session_id=$1`, f.Session).Scan(&acknowledged); err != nil {
					return err
				}
				if !acknowledged {
					return errors.New("guest receipt retired before durable acknowledgment")
				}
				caller := agent.Caller{Kind: "user", ID: f.User}
				req := agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: kind}
				switch index {
				case 0:
					req.Kind = "interrupt"
					var err error
					hold, err = agent.ControlSession(t.Context(), f.Pool, caller, req)
					if err != nil {
						return err
					}
				case 1:
					req.Kind = "resume"
					req.HoldID = hold.HoldID
					if _, err := agent.ControlSession(t.Context(), f.Pool, caller, req); err != nil {
						return err
					}
				case 2:
					req.Kind = "cancel"
					if _, err := agent.ControlSession(t.Context(), f.Pool, caller, req); err != nil {
						return err
					}
				}
			}
			if err := frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: identity, AttachmentSequence: attach.AttachmentSequence, EventSequence: 5, Message: &agentv1.GuestSessionMessage_Stopped{Stopped: &agentv1.SessionStopped{}}}); err != nil {
				return err
			}
			var ack agentv1.HostSessionMessage
			if err := frameio.ReadProtoFrameBounded(stream, 1<<20, &ack); err != nil {
				return err
			}
			if ack.GetAcknowledged().GetThroughSequence() != 5 {
				return errors.New("physical stop not acknowledged")
			}
			return nil
		}
		finished <- run()
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	grant := &agentv1.SessionGrant{Identity: identity, ComputerId: f.Computer.String(), ComputerInstanceId: "instance", WriterGeneration: 1, WorkerHostId: f.Worker.String(), ComputerLeaseEpoch: 1, AuthorityGeneration: attachment.AuthorityGeneration, ExpiresAtUnixNano: attachment.ExpiresAt.UnixNano(), ChannelCredential: "private"}
	connection, attached, err := computerhost.OpenAgentSession(ctx, machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: uint64(attachment.AttachmentSequence)})
	if err != nil {
		t.Fatal(err)
	}
	err = computerhost.ServeControlledAgentSession(ctx, connection, attached, f.Environment.String(), client, func(context.Context, *agentv1.GuestSessionMessage) error {
		return errors.New("unexpected observer event")
	})
	if err != nil {
		t.Fatalf("clean physical stop: %v", err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	var stopped bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='stopped' AND fenced_at IS NOT NULL FROM session_processes WHERE session_id=$1`, f.Session).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("physical stop: %v %v", stopped, err)
	}
}

func TestWorkerAgentSupersededControlReceiptKeepsCurrentAttachment(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	a, err := client.AcquireAgentAttachment(t.Context(), runtimeTestSession(f))
	if err != nil {
		t.Fatal(err)
	}
	request := workerapi.AgentControlRequest{Session: runtimeTestSession(f), AttachmentSequence: a.AttachmentSequence}
	first, err := client.PrepareAgentControl(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.ControlSession(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "hold-before-receipt", Kind: "interrupt"}); err != nil {
		t.Fatal(err)
	}
	if err := client.AcknowledgeAgentControl(t.Context(), workerapi.AgentControlReceipt{Session: request.Session, AttachmentSequence: request.AttachmentSequence, Sequence: first.Sequence, AuthorityGeneration: first.AuthorityGeneration, Kind: first.Kind}); err != nil {
		t.Fatalf("supersession broke attachment: %v", err)
	}
	current, err := client.PrepareAgentControl(t.Context(), request)
	if err != nil || current.Kind != "suspend" || current.Sequence != first.Sequence+1 || current.Acknowledged {
		t.Fatalf("did not converge on same attachment: %+v %v", current, err)
	}
	request.AttachmentSequence++
	_, err = client.PrepareAgentControl(t.Context(), request)
	var rejected interface{ SessionAuthorityRejected() bool }
	if !errors.As(err, &rejected) || !rejected.SessionAuthorityRejected() {
		t.Fatalf("wrong attachment did not reject authority: %v", err)
	}
}
