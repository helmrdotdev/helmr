package computerhost

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type terminalControlClient struct {
	logs func(context.Context, workerapi.SessionLogRequest) (workerapi.DiagnosticLogReceipt, error)
	agentRuntimeTestClient
	ready   func(context.Context, workerapi.AgentControlRequest) error
	stopped bool
	failure func(context.Context, workerapi.AgentControlRequest) error
	prepare func(context.Context, workerapi.AgentControlRequest) (workerapi.AgentControlResponse, error)
}

func (c *terminalControlClient) AppendSessionLog(ctx context.Context, request workerapi.SessionLogRequest) (workerapi.DiagnosticLogReceipt, error) {
	if c.logs != nil {
		return c.logs(ctx, request)
	}
	return workerapi.DiagnosticLogReceipt{}, errors.New("unexpected diagnostic acceptance")
}

func (c *terminalControlClient) PrepareAgentControl(ctx context.Context, r workerapi.AgentControlRequest) (workerapi.AgentControlResponse, error) {
	if c.prepare != nil {
		return c.prepare(ctx, r)
	}
	return workerapi.AgentControlResponse{Sequence: 1, AuthorityGeneration: 3, Kind: "shutdown", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (c *terminalControlClient) ObserveAgentReady(ctx context.Context, r workerapi.AgentControlRequest) error {
	if c.ready != nil {
		return c.ready(ctx, r)
	}
	return nil
}

func (*terminalControlClient) AcknowledgeAgentControl(context.Context, workerapi.AgentControlReceipt) error {
	return errors.New("unexpected new control receipt")
}
func (c *terminalControlClient) ObserveAgentStopped(context.Context, workerapi.AgentControlRequest) error {
	c.stopped = true
	return nil
}
func TestAgentControlsTerminalAttachmentDrainsBeforePhysicalStop(t *testing.T) {
	grant := sessionTransportGrant()
	client := &terminalControlClient{}
	// Reconciliation must not issue a fresh shutdown on a terminal attachment.
	if err := (&agentSessionControls{terminal: true, client: client}).apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	machine := sessionTransportMachine(t, func(stream net.Conn, attach *agentv1.SessionAttach) {
		for sequence := uint64(1); sequence <= 3; sequence++ {
			message := &agentv1.GuestSessionMessage{Identity: grant.Identity, AttachmentSequence: attach.AttachmentSequence, EventSequence: sequence}
			if sequence < 3 {
				message.EventSequence = 0
				message.Message = &agentv1.GuestSessionMessage_Log{Log: &agentv1.SessionLog{Stream: agentv1.SessionLog_STREAM_STDOUT, Kind: agentv1.SessionLog_KIND_DATA, Sequence: int64(sequence), ThroughSequence: int64(sequence), ObservedAtUnixNano: 1, Data: []byte("log")}}
			} else {
				message.Message = &agentv1.GuestSessionMessage_Stopped{Stopped: &agentv1.SessionStopped{}}
			}
			if err := frameio.WriteProtoFrame(stream, message); err != nil {
				t.Error(err)
				return
			}
			var ack agentv1.HostSessionMessage
			if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &ack); err != nil {
				t.Error(err)
				return
			}
			if (sequence < 3 && ack.GetLogAcknowledged().GetThroughSequence() != int64(sequence)) || (sequence == 3 && ack.GetAcknowledged().GetThroughSequence() != sequence) {
				t.Errorf("replay interrupted by %v", &ack)
				return
			}
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	connection, attached, err := OpenAgentSession(ctx, machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 2})
	if err != nil {
		t.Fatal(err)
	}
	attached.Terminal = true
	observed := 0
	client.logs = func(_ context.Context, request workerapi.SessionLogRequest) (workerapi.DiagnosticLogReceipt, error) {
		observed++
		if client.stopped {
			t.Error("fenced before actual stop event")
		}
		accepted := time.Now().UTC()
		return workerapi.DiagnosticLogReceipt{ThroughSequence: request.ThroughSequence, AcceptedAt: accepted, ExpiresAt: accepted.Add(90 * 24 * time.Hour)}, nil
	}
	err = ServeControlledAgentSession(ctx, connection, attached, "environment", client, func(context.Context, *agentv1.GuestSessionMessage) error {
		return errors.New("unexpected unowned event")
	})
	if err != nil {
		t.Fatalf("clean terminal drain: %v", err)
	}
	if observed != 2 || !client.stopped {
		t.Fatalf("replay count=%d stopped=%v", observed, client.stopped)
	}
}

type stoppedPrepareRejection struct{}

func (stoppedPrepareRejection) Error() string                  { return "process is stopped" }
func (stoppedPrepareRejection) SessionAuthorityRejected() bool { return true }
func TestAgentControlsStopOvertakesPendingPrepare(t *testing.T) {
	grant := sessionTransportGrant()
	machine := sessionTransportMachine(t, func(stream net.Conn, _ *agentv1.SessionAttach) { _, _ = io.Copy(io.Discard, stream) })
	connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	started, reply := make(chan struct{}), make(chan struct{})
	client := &terminalControlClient{prepare: func(ctx context.Context, _ workerapi.AgentControlRequest) (workerapi.AgentControlResponse, error) {
		close(started)
		select {
		case <-reply:
		case <-ctx.Done():
			return workerapi.AgentControlResponse{}, ctx.Err()
		}
		return workerapi.AgentControlResponse{}, stoppedPrepareRejection{}
	}}
	owner := &agentSessionControls{connection: connection, environment: "environment", client: client}
	done := make(chan error, 1)
	go func() { done <- owner.apply(t.Context()) }()
	<-started
	if handled, err := owner.observe(t.Context(), &agentv1.GuestSessionMessage{Message: &agentv1.GuestSessionMessage_Stopped{Stopped: &agentv1.SessionStopped{}}}); err != nil || !handled {
		t.Fatalf("stop: %v %v", handled, err)
	}
	close(reply)
	if err := <-done; err != nil {
		t.Fatalf("late prepare rejection replaced clean stop: %v", err)
	}
}

func (c *terminalControlClient) ObserveAgentFailure(ctx context.Context, r workerapi.AgentControlRequest) error {
	if c.failure != nil {
		return c.failure(ctx, r)
	}
	return errors.New("unexpected runtime failure")
}

func TestAgentControlsRetainedFailureSurvivesExitDuringReport(t *testing.T) {
	for _, transportExit := range []bool{false, true} {
		name := "cancel"
		if transportExit {
			name = "transport"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				grant := sessionTransportGrant()
				machine := sessionTransportMachine(t, func(stream net.Conn, _ *agentv1.SessionAttach) { _, _ = io.Copy(io.Discard, stream) })
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				connection, attached, err := OpenAgentSession(ctx, machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 2})
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				started, release := make(chan struct{}), make(chan struct{})
				reportFailure := errors.New("failure report response unavailable")
				client := &terminalControlClient{
					prepare: func(context.Context, workerapi.AgentControlRequest) (workerapi.AgentControlResponse, error) {
						return workerapi.AgentControlResponse{Sequence: 1, AuthorityGeneration: 3, Kind: "resume", Error: "retained runtime rejection", ExpiresAt: time.Now().Add(time.Hour)}, nil
					},
					failure: func(ctx context.Context, r workerapi.AgentControlRequest) error {
						close(started)
						<-release
						if ctx.Err() != nil {
							t.Errorf("report inherited cancellation: %v", ctx.Err())
						}
						return reportFailure
					},
				}
				done := make(chan error, 1)
				go func() {
					done <- ServeControlledAgentSession(ctx, connection, attached, "environment", client, func(context.Context, *agentv1.GuestSessionMessage) error { return nil })
				}()
				<-started
				if transportExit {
					_ = connection.Close()
				} else {
					cancel()
				}
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("returned without joining report: %v", err)
				default:
				}
				close(release)
				err = <-done
				var failed SessionControlFailedError
				if !errors.As(err, &failed) || failed.AttachmentSequence != 2 || !errors.Is(failed.FailureError, reportFailure) {
					t.Fatalf("lost retained failure: %+v %v", failed, err)
				}
			})
		})
	}
}

func TestAgentControlsRetainedErrorSurvivesSupersession(t *testing.T) {
	for _, reattached := range []bool{false, true} {
		name := "newer-control"
		if reattached {
			name = "new-attachment"
		}
		t.Run(name, func(t *testing.T) {
			grant := sessionTransportGrant()
			machine := sessionTransportMachine(t, func(stream net.Conn, _ *agentv1.SessionAttach) { _, _ = io.Copy(io.Discard, stream) })
			connection, _, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 2})
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			reports := 0
			client := &terminalControlClient{failure: func(_ context.Context, r workerapi.AgentControlRequest) error {
				reports++
				if r.AttachmentSequence != 2 || r.Session.ProcessEpoch != grant.Identity.ProcessEpoch {
					t.Fatalf("wrong retained failure identity %+v", r)
				}
				return nil
			}}
			owner := &agentSessionControls{connection: connection, environment: "environment", client: client}
			if !reattached {
				owner.latest = workerapi.AgentControlResponse{Sequence: 2, AuthorityGeneration: 4, Kind: "suspend"}
			}
			result := &agentv1.DeliveryResult{DeliveryId: "control:1:3:resume", Outcome: &agentv1.DeliveryResult_ValueJson{ValueJson: []byte("null")}}
			message := &agentv1.GuestSessionMessage{Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: result}}}}
			if handled, err := owner.observe(t.Context(), message); err != nil || !handled || reports != 0 {
				t.Fatalf("old success %+v %v reports=%d", handled, err, reports)
			}
			result.Outcome = &agentv1.DeliveryResult_Error{Error: &agentv1.OperationError{Code: "reconstruction_required", Message: "native continuation unavailable"}}
			handled, err := owner.observe(t.Context(), message)
			var failed SessionControlFailedError
			if !handled || !errors.As(err, &failed) || reports != 1 || failed.FailureError != nil {
				t.Fatalf("retained error ignored: %+v %v reports=%d", failed, err, reports)
			}
		})
	}
}

func TestAgentReadyWaitsForDurableObservation(t *testing.T) {
	grant := &agentv1.SessionGrant{Identity: &agentv1.SessionIdentity{SessionId: "session", ProcessEpoch: 1}, ComputerLeaseEpoch: 3}
	connection := &AgentSessionConnection{identity: grant.Identity, grant: grant, attachment: 4}
	uncertain := true
	calls := 0
	client := &terminalControlClient{ready: func(_ context.Context, r workerapi.AgentControlRequest) error {
		calls++
		if r.Session.EnvironmentID != "environment" || r.Session.SessionID != "session" || r.Session.ComputerLeaseEpoch != 3 || r.AttachmentSequence != 4 {
			t.Fatalf("readiness identity changed: %+v", r)
		}
		if uncertain {
			return io.ErrUnexpectedEOF
		}
		return nil
	}}
	owner := &agentSessionControls{connection: connection, client: client, environment: "environment"}
	event := &agentv1.GuestSessionMessage{Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Event: &agentv1.ProgramEvent_Ready{Ready: &agentv1.SessionReady{}}}}}
	if handled, err := owner.observe(t.Context(), event); !handled || !errors.Is(err, io.ErrUnexpectedEOF) || owner.ready {
		t.Fatalf("uncertain readiness consumed: %v %v ready=%v", handled, err, owner.ready)
	}
	uncertain = false
	if handled, err := owner.observe(t.Context(), event); !handled || err != nil || !owner.ready || calls != 2 {
		t.Fatalf("readiness reconciliation: %v %v ready=%v calls=%d", handled, err, owner.ready, calls)
	}
}
