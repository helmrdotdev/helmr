package computerhost

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/ids"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type AllocatedSessionClient interface {
	AgentStartClient
	AgentControlClient
	AgentTurnClient
	AgentMessageClient
	ListComputerProcesses(context.Context, workerapi.AllocationIdentity, *workerapi.ProcessIdentity) (workerapi.ComputerProcessesResponse, error)
}

// ServeAllocatedSessions owns attachment jobs for a fresh active Computer. The
// caller retains the physical machine and renews its lease until this function
// has joined. Cancellation closes transports, never guest processes or the VM.
// Whole-Computer restoration first uses AgentComputerOwner to install and
// activate retained members before starting this ordinary discovery loop.
func (p *PreparedMachines) ServeAllocatedSessions(ctx context.Context, machine vm.GuestControlMachine, allocation workerapi.ComputerAllocationDelivery, hostID string, client AllocatedSessionClient, observe func(context.Context, *agentv1.GuestSessionMessage) error) error {
	identity := allocation.Identity
	if p == nil || machine == nil || client == nil || observe == nil || identity.Kind != "computer" || identity.Epoch <= 0 || allocation.ChannelCredential == "" {
		return errors.New("session discovery requires an owned Computer and authenticated client")
	}
	for _, id := range []string{identity.EnvironmentID, identity.OwnerID, identity.InstanceID, hostID} {
		if ids.Validate(id) != nil {
			return errors.New("invalid Computer allocation identity")
		}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	var jobs sync.WaitGroup
	defer func() { cancel(context.Canceled); jobs.Wait() }()
	// Discovery alone never retires a job. Its own authenticated attachment and
	// controls decide when the process has stopped. Completed jobs remain recorded
	// until a complete scan no longer lists them, preventing stale page replay.
	owned := make(map[workerapi.ProcessIdentity]<-chan struct{})
	for ctx.Err() == nil {
		seen := make(map[workerapi.ProcessIdentity]bool)
		var cursor *workerapi.ProcessIdentity
		var scanErr error
		for {
			requestCtx, requestCancel := context.WithTimeout(ctx, 10*time.Second)
			page, err := client.ListComputerProcesses(requestCtx, identity, cursor)
			requestCancel()
			if err != nil {
				scanErr = err
				break
			}
			if len(page.Processes) == 0 {
				break
			}
			for _, process := range page.Processes {
				if ids.Validate(process.SessionID) != nil || process.Epoch <= 0 || seen[process] {
					return errors.New("invalid or repeated Session discovery identity")
				}
				seen[process] = true
				if owned[process] != nil {
					continue
				}
				done := make(chan struct{})
				owned[process] = done
				jobs.Go(func() {
					defer close(done)
					grant := &agentv1.SessionGrant{Identity: &agentv1.SessionIdentity{SessionId: process.SessionID, ProcessEpoch: process.Epoch}, ComputerId: identity.OwnerID, ComputerInstanceId: identity.InstanceID, WriterGeneration: identity.Epoch, WorkerHostId: hostID, ComputerLeaseEpoch: identity.Epoch, ChannelCredential: allocation.ChannelCredential}
					retryDelay := 250 * time.Millisecond
					for ctx.Err() == nil {
						connection, attached, err := p.StartAllocatedAgentSession(ctx, machine, identity.EnvironmentID, grant, client)
						if err == nil {
							servingSince := time.Now()
							err = ServeExecutingAgentSession(ctx, connection, attached, identity.EnvironmentID, client, observe)
							// An attached transport can immediately replay an event
							// whose durable observation is still unavailable.
							if time.Since(servingSince) >= 10*time.Second {
								retryDelay = 250 * time.Millisecond
							}
							if err == nil {
								return
							}
						}
						var failed SessionControlFailedError
						var hostRejected interface{ WorkerAuthorityRejected() bool }
						if errors.As(err, &hostRejected) && hostRejected.WorkerAuthorityRejected() {
							cancel(err)
							return
						}
						if errors.As(err, &failed) {
							// Control service records the failure before returning it.
							// Reattachment reconciles a lost failure receipt or the
							// resulting shutdown; peer transports keep their lifetimes.
							p.logInfo("Session control failure requires reconciliation", "session_id", process.SessionID, "process_epoch", process.Epoch, "error", failed)
						}
						if errors.Is(err, ErrAgentSessionStopped) {
							return
						}
						// Startup/attachment rejection can be temporary (hold,
						// capture or a pending save). Only explicit physical-stop
						// authority above retires this assignment.
						// Held startup can remain blocked for hours. Bound durable
						// attachment writes and spread outage retries across peers.
						wait := retryDelay/2 + time.Duration(rand.Int64N(int64(retryDelay/2)))
						if sleepWithContext(ctx, wait) != nil {
							return
						}
						retryDelay = min(10*time.Second, retryDelay*2)
					}
				})
			}
			last := page.Processes[len(page.Processes)-1]
			cursor = &last
		}
		if scanErr == nil {
			for process, done := range owned {
				if !seen[process] {
					select {
					case <-done:
						delete(owned, process)
					default:
					}
				}
			}
		} else {
			var closed *httpclient.Error
			if errors.As(scanErr, &closed) && closed.Code == workerapi.AllocationClosed {
				return scanErr
			}
			var rejected interface{ WorkerAuthorityRejected() bool }
			if errors.As(scanErr, &rejected) && rejected.WorkerAuthorityRejected() {
				return scanErr
			}
		}
		if sleepWithContext(ctx, time.Second) != nil {
			break
		}
	}
	return context.Cause(ctx)
}
