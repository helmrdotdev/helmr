package computerhost

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

type allocatedSessionTestMachine struct{ *agentControlMachine }

func (m allocatedSessionTestMachine) WithRunningGuestControl(ctx context.Context, _ vm.GuestControlStage, run func(context.Context) error) error {
	return run(ctx)
}

type allocatedSessionTestClient struct {
	retainedSessionStartClient
	*terminalControlClient
	mu        sync.Mutex
	sequences map[string]int64
	processes []workerapi.ProcessIdentity
	lists     int
	authorize func(context.Context, workerapi.AgentControlRequest) (workerapi.AgentStartResponse, error)
	isStopped func(string) bool
	renewals  atomic.Int64
}

func (c *allocatedSessionTestClient) NextAgentTurn(context.Context, workerapi.AgentTurnRequest) (*workerapi.AgentTurnDispatch, error) {
	return nil, nil
}
func (c *allocatedSessionTestClient) ObserveAgentTurn(context.Context, workerapi.AgentTurnReceipt) (workerapi.AgentTurnAcknowledgment, error) {
	return workerapi.AgentTurnAcknowledgment{}, errors.New("unexpected Turn receipt")
}

func (c *allocatedSessionTestClient) AcquireAgentAttachment(_ context.Context, s workerapi.RuntimeSession) (workerapi.AgentAttachmentResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sequences[s.SessionID]++
	if c.isStopped != nil && c.isStopped(s.SessionID) {
		return workerapi.AgentAttachmentResponse{Stopped: true}, nil
	}
	return workerapi.AgentAttachmentResponse{Starting: true, AttachmentSequence: c.sequences[s.SessionID], AuthorityGeneration: 4, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (c *allocatedSessionTestClient) AuthorizeAgentStart(ctx context.Context, r workerapi.AgentControlRequest) (workerapi.AgentStartResponse, error) {
	if c.authorize != nil {
		return c.authorize(ctx, r)
	}
	return c.retainedSessionStartClient.AuthorizeAgentStart(ctx, r)
}
func (c *allocatedSessionTestClient) RenewAgentAuthority(context.Context, workerapi.RuntimeSession) (workerapi.AgentAuthorityResponse, error) {
	c.renewals.Add(1)
	return workerapi.AgentAuthorityResponse{AuthorityGeneration: 4, ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (c *allocatedSessionTestClient) ListComputerProcesses(_ context.Context, _ workerapi.AllocationIdentity, after *workerapi.ProcessIdentity) (workerapi.ComputerProcessesResponse, error) {
	c.lists++
	if c.lists == 4 {
		return workerapi.ComputerProcessesResponse{}, errors.New("temporary discovery outage")
	}
	index := 0
	if after != nil {
		index = len(c.processes)
		for i, p := range c.processes {
			if p == *after {
				index = i + 1
				break
			}
		}
	}
	if index == len(c.processes) {
		return workerapi.ComputerProcessesResponse{}, nil
	}
	return workerapi.ComputerProcessesResponse{Processes: c.processes[index : index+1]}, nil
}

func TestAllocatedSessionsPaginateReconnectAndJoinWithoutRestartingPeers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := uuid.NewV7().String(), uuid.NewV7().String()
		client := &allocatedSessionTestClient{retainedSessionStartClient: retainedSessionStartClient{t}, terminalControlClient: &terminalControlClient{prepare: func(context.Context, workerapi.AgentControlRequest) (workerapi.AgentControlResponse, error) {
			return workerapi.AgentControlResponse{}, errors.New("control temporarily unavailable")
		}}, sequences: make(map[string]int64), processes: []workerapi.ProcessIdentity{{SessionID: first, Epoch: 1}, {SessionID: second, Epoch: 2}}}
		allocation := workerapi.ComputerAllocationDelivery{Identity: workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: uuid.NewV7().String(), OwnerID: uuid.NewV7().String(), InstanceID: uuid.NewV7().String(), Epoch: 3}, ChannelCredential: "owned-channel"}
		host := uuid.NewV7().String()
		var active atomic.Int64
		machine := sessionTransportMachine(t, func(stream net.Conn, r *agentv1.SessionAttach) {
			active.Add(1)
			defer active.Add(-1)
			if r.Start != nil || r.Grant.ComputerInstanceId != allocation.Identity.InstanceID || r.Grant.ComputerId != allocation.Identity.OwnerID || r.Grant.WorkerHostId != host || r.Grant.ComputerLeaseEpoch != 3 || r.Grant.WriterGeneration != 3 || r.Grant.ChannelCredential != allocation.ChannelCredential {
				t.Error("attachment lost physical identity or reran setup")
			}
			if r.Grant.Identity.SessionId == first && r.AttachmentSequence == 1 {
				return
			}
			_, _ = io.Copy(io.Discard, stream)
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		machines := PreparedMachines{SessionLogLimits: &agentv1.SessionLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 2}}
		go func() {
			done <- machines.ServeAllocatedSessions(ctx, allocatedSessionTestMachine{machine}, allocation, host, client, func(context.Context, *agentv1.GuestSessionMessage) error { return nil })
		}()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		client.mu.Lock()
		if client.sequences[first] != 2 || client.sequences[second] != 1 {
			t.Errorf("reconnect disturbed peer: %+v", client.sequences)
		}
		client.mu.Unlock()
		if active.Load() != 2 {
			t.Errorf("active attachments: %d", active.Load())
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		synctest.Wait()
		if active.Load() != 0 {
			t.Fatal("attachment jobs survived physical owner cancellation")
		}
	})
}

func TestAllocatedSessionsLocalDenialAndFailurePreservePeerRenewal(t *testing.T) {
	for _, scenario := range []string{"startup denial", "persistent startup denial", "control failure"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				bad, peer := uuid.NewV7().String(), uuid.NewV7().String()
				var released, admitted, failed atomic.Bool
				client := &allocatedSessionTestClient{retainedSessionStartClient: retainedSessionStartClient{t}, terminalControlClient: &terminalControlClient{}, sequences: make(map[string]int64), processes: []workerapi.ProcessIdentity{{SessionID: bad, Epoch: 1}, {SessionID: peer, Epoch: 1}}}
				client.authorize = func(context.Context, workerapi.AgentControlRequest) (workerapi.AgentStartResponse, error) {
					if !released.Load() {
						return workerapi.AgentStartResponse{}, workerclient.SessionAuthorityRejectedError{Err: &httpclient.Error{StatusCode: http.StatusConflict}}
					}
					admitted.Store(true)
					// Startup artifact execution is covered by the Linux transfer test.
					// This fixture stops at renewed admission after the temporary hold.
					return workerapi.AgentStartResponse{}, ErrAgentSessionStopped
				}
				client.prepare = func(_ context.Context, r workerapi.AgentControlRequest) (workerapi.AgentControlResponse, error) {
					if scenario == "control failure" && r.Session.SessionID == bad {
						return workerapi.AgentControlResponse{Sequence: 1, AuthorityGeneration: 4, Kind: "suspend", Error: "retained control failure", ExpiresAt: time.Now().Add(time.Minute)}, nil
					}
					return workerapi.AgentControlResponse{}, errors.New("temporary control outage")
				}
				client.failure = func(_ context.Context, r workerapi.AgentControlRequest) error {
					if r.Session.SessionID != bad {
						t.Error("peer failure recorded")
					}
					failed.Store(true)
					return nil
				}
				client.isStopped = func(id string) bool { return id == bad && failed.Load() }
				machine := allocatedSessionTestMachine{&agentControlMachine{handle: func(stream net.Conn) {
					if _, _, err := wire.ReadStreamFrameHeader(stream); err != nil {
						t.Error(err)
						return
					}
					var r agentv1.SessionAttach
					if err := frameio.ReadProtoFrame(stream, &r); err != nil {
						t.Error(err)
						return
					}
					message := &agentv1.GuestSessionMessage{Identity: r.Grant.Identity, AttachmentSequence: r.AttachmentSequence}
					if scenario != "control failure" && r.Grant.Identity.SessionId == bad {
						message.Message = &agentv1.GuestSessionMessage_Absent{Absent: &agentv1.SessionAbsent{}}
					} else {
						message.Message = &agentv1.GuestSessionMessage_Attached{Attached: &agentv1.SessionAttached{Ready: true}}
					}
					if err := frameio.WriteProtoFrame(stream, message); err != nil {
						t.Error(err)
						return
					}
					_, _ = io.Copy(io.Discard, stream)
				}}}
				allocation := workerapi.ComputerAllocationDelivery{Identity: workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: uuid.NewV7().String(), OwnerID: uuid.NewV7().String(), InstanceID: uuid.NewV7().String(), Epoch: 1}, ChannelCredential: "owned"}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				machines := PreparedMachines{SessionLogLimits: &agentv1.SessionLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 2}}
				go func() {
					done <- machines.ServeAllocatedSessions(ctx, machine, allocation, uuid.NewV7().String(), client, func(context.Context, *agentv1.GuestSessionMessage) error { return nil })
				}()
				time.Sleep(time.Second)
				if scenario != "persistent startup denial" {
					released.Store(true)
				}
				time.Sleep(34 * time.Second)
				synctest.Wait()
				if scenario == "startup denial" && !admitted.Load() {
					t.Error("held assignment never retried admission")
				}
				if scenario == "control failure" && !failed.Load() {
					t.Error("local failure was not recorded")
				}
				client.mu.Lock()
				peerAttachments, badAttachments := client.sequences[peer], client.sequences[bad]
				client.mu.Unlock()
				if scenario == "persistent startup denial" {
					if badAttachments < 2 || badAttachments > 20 || admitted.Load() {
						t.Errorf("held startup retry load: attempts=%d admitted=%v", badAttachments, admitted.Load())
					}
					released.Store(true)
					time.Sleep(11 * time.Second)
					synctest.Wait()
					if !admitted.Load() {
						t.Error("backoff failed to retry after hold released")
					}
				}
				if peerAttachments != 1 || client.renewals.Load() == 0 {
					t.Errorf("peer interrupted: attachments=%d renewals=%d", peerAttachments, client.renewals.Load())
				}
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("Session-local event terminated Computer owner: %v", err)
				}
			})
		})
	}
}

type retainedFailureSessionClient struct {
	*allocatedSessionTestClient
	stopped atomic.Bool
}

func (c *retainedFailureSessionClient) ObserveAgentStopped(context.Context, workerapi.AgentControlRequest) error {
	c.stopped.Store(true)
	return nil
}

func TestAllocatedSessionsRetainedFailureAcknowledgedOnlyAfterDurableReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bad, peer := uuid.NewV7().String(), uuid.NewV7().String()
		var reports atomic.Int64
		var acknowledged, shutdown atomic.Bool
		client := &retainedFailureSessionClient{allocatedSessionTestClient: &allocatedSessionTestClient{
			retainedSessionStartClient: retainedSessionStartClient{t},
			terminalControlClient:      &terminalControlClient{}, sequences: make(map[string]int64),
			processes: []workerapi.ProcessIdentity{{SessionID: bad, Epoch: 1}, {SessionID: peer, Epoch: 1}},
		}}
		client.prepare = func(_ context.Context, r workerapi.AgentControlRequest) (workerapi.AgentControlResponse, error) {
			if r.Session.SessionID == bad && acknowledged.Load() {
				return workerapi.AgentControlResponse{Sequence: 2, AuthorityGeneration: 4, Kind: "shutdown", ExpiresAt: time.Now().Add(time.Minute)}, nil
			}
			return workerapi.AgentControlResponse{}, errors.New("temporary control outage")
		}
		client.failure = func(_ context.Context, r workerapi.AgentControlRequest) error {
			if r.Session.SessionID != bad {
				t.Error("failed the peer")
			}
			if reports.Add(1) == 1 {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		machine := allocatedSessionTestMachine{sessionTransportMachine(t, func(stream net.Conn, r *agentv1.SessionAttach) {
			if r.Grant.Identity.SessionId == peer {
				_, _ = io.Copy(io.Discard, stream)
				return
			}
			if !acknowledged.Load() {
				message := &agentv1.GuestSessionMessage{Identity: r.Grant.Identity, AttachmentSequence: r.AttachmentSequence, EventSequence: 1,
					Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: r.Grant.Identity, Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: &agentv1.DeliveryResult{DeliveryId: "control:1:3:resume", Outcome: &agentv1.DeliveryResult_Error{Error: &agentv1.OperationError{Code: "reconstruction_required", Message: "native continuation unavailable"}}}}}}}
				if err := frameio.WriteProtoFrame(stream, message); err != nil {
					t.Error(err)
					return
				}
				var ack agentv1.HostSessionMessage
				if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &ack); err != nil {
					return // An uncertain report retains the event for the next attachment.
				}
				if ack.GetAcknowledged().GetThroughSequence() != 1 || reports.Load() < 2 {
					t.Errorf("failure acknowledged before durable report: %v reports=%d", &ack, reports.Load())
					return
				}
				acknowledged.Store(true)
				_, _ = io.Copy(io.Discard, stream)
				return
			}
			for {
				var command agentv1.HostSessionMessage
				if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &command); err != nil {
					return
				}
				if command.GetCommand().GetShutdown() == nil {
					continue
				}
				shutdown.Store(true)
				if err := frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: r.Grant.Identity, AttachmentSequence: r.AttachmentSequence, EventSequence: 2, Message: &agentv1.GuestSessionMessage_Stopped{Stopped: &agentv1.SessionStopped{}}}); err != nil {
					t.Error(err)
					return
				}
				var ack agentv1.HostSessionMessage
				if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &ack); err != nil || ack.GetAcknowledged().GetThroughSequence() != 2 {
					t.Errorf("physical stop receipt not drained: %v %v", &ack, err)
				}
				return
			}
		})}
		allocation := workerapi.ComputerAllocationDelivery{Identity: workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: uuid.NewV7().String(), OwnerID: uuid.NewV7().String(), InstanceID: uuid.NewV7().String(), Epoch: 1}, ChannelCredential: "owned"}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			machines := PreparedMachines{SessionLogLimits: &agentv1.SessionLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 2}}
			done <- machines.ServeAllocatedSessions(ctx, machine, allocation, uuid.NewV7().String(), client, func(context.Context, *agentv1.GuestSessionMessage) error { return nil })
		}()
		time.Sleep(35 * time.Second)
		synctest.Wait()
		client.mu.Lock()
		badAttachments, peerAttachments := client.sequences[bad], client.sequences[peer]
		client.mu.Unlock()
		if reports.Load() != 2 || !acknowledged.Load() || !shutdown.Load() || !client.stopped.Load() || badAttachments != 3 || peerAttachments != 1 || client.renewals.Load() == 0 {
			t.Errorf("failure drain: reports=%d ack=%v shutdown=%v stopped=%v attachments=%d peer=%d renewals=%d", reports.Load(), acknowledged.Load(), shutdown.Load(), client.stopped.Load(), badAttachments, peerAttachments, client.renewals.Load())
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestAllocatedSessionsRetainedReadyOutageBacksOffAfterAttachment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bad, peer := uuid.NewV7().String(), uuid.NewV7().String()
		var available, acknowledged atomic.Bool
		var observations atomic.Int64
		client := &allocatedSessionTestClient{retainedSessionStartClient: retainedSessionStartClient{t}, terminalControlClient: &terminalControlClient{}, sequences: make(map[string]int64), processes: []workerapi.ProcessIdentity{{SessionID: bad, Epoch: 1}, {SessionID: peer, Epoch: 1}}}
		client.prepare = func(context.Context, workerapi.AgentControlRequest) (workerapi.AgentControlResponse, error) {
			return workerapi.AgentControlResponse{}, errors.New("temporary control outage")
		}
		client.ready = func(_ context.Context, r workerapi.AgentControlRequest) error {
			if r.Session.SessionID != bad {
				t.Error("unexpected peer readiness")
			}
			observations.Add(1)
			if !available.Load() {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		machine := allocatedSessionTestMachine{sessionTransportMachine(t, func(stream net.Conn, r *agentv1.SessionAttach) {
			if r.Grant.Identity.SessionId == peer {
				_, _ = io.Copy(io.Discard, stream)
				return
			}
			message := &agentv1.GuestSessionMessage{Identity: r.Grant.Identity, AttachmentSequence: r.AttachmentSequence, EventSequence: 1, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: r.Grant.Identity, Event: &agentv1.ProgramEvent_Ready{Ready: &agentv1.SessionReady{}}}}}
			if err := frameio.WriteProtoFrame(stream, message); err != nil {
				t.Error(err)
				return
			}
			var ack agentv1.HostSessionMessage
			if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &ack); err != nil {
				return
			}
			if ack.GetAcknowledged().GetThroughSequence() != 1 || !available.Load() {
				t.Errorf("readiness ACK before durable observation: %v", &ack)
				return
			}
			acknowledged.Store(true)
			_, _ = io.Copy(io.Discard, stream)
		})}
		allocation := workerapi.ComputerAllocationDelivery{Identity: workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: uuid.NewV7().String(), OwnerID: uuid.NewV7().String(), InstanceID: uuid.NewV7().String(), Epoch: 1}, ChannelCredential: "owned"}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			machines := PreparedMachines{SessionLogLimits: &agentv1.SessionLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 2}}
			done <- machines.ServeAllocatedSessions(ctx, machine, allocation, uuid.NewV7().String(), client, func(context.Context, *agentv1.GuestSessionMessage) error { return nil })
		}()
		time.Sleep(35 * time.Second)
		synctest.Wait()
		if count := observations.Load(); count < 2 || count > 20 || acknowledged.Load() {
			t.Fatalf("unavailable retained readiness retry load: %d", count)
		}
		available.Store(true)
		time.Sleep(11 * time.Second)
		synctest.Wait()
		client.mu.Lock()
		peerAttachments := client.sequences[peer]
		client.mu.Unlock()
		if !acknowledged.Load() || peerAttachments != 1 || client.renewals.Load() == 0 {
			t.Fatal("readiness did not recover or disturbed peer")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func (c *allocatedSessionTestClient) NextAgentMessage(context.Context, workerapi.AgentMessageRequest) (*workerapi.AgentMessageDispatch, error) {
	return nil, nil
}
func (c *allocatedSessionTestClient) ObserveAgentMessage(context.Context, workerapi.AgentMessageReceipt) error {
	return nil
}
