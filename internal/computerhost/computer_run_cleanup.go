package computerhost

import (
	"context"
	"errors"
	"fmt"
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// The Computer owner continues cleanup after a Run's own context/stream closes.
// A failed CP acknowledgement is retried with the same Guest receipt; inability
// to prove scoped termination instead requires the physical owner's failure path.
func (m Server) reconcileComputerRuns(ctx context.Context, machine vm.Machine, mount workerapi.ComputerInstanceAssignment, checkout *machineCheckout, client workerapi.ComputerServerControlPlaneClient) error {
	request := workerapi.ComputerRunCleanupRequest{EnvironmentID: mount.EnvironmentID, ComputerInstanceID: mount.ComputerInstanceID, WriterGeneration: mount.WriterGeneration}
	var failedLease string
	var failures int
	var cleanupErr error
	var cleanupVersion int64
	for {
		response, err := client.GetComputerRunCleanup(ctx, request)
		if err == nil {
			if response.Run == nil || response.Run.RunLeaseID != failedLease {
				failedLease = ""
				failures = 0
				cleanupErr = nil
			}
			version, held := checkout.runCleanupHeld(cleanupVersion, false)
			cleanupVersion = version
			if held {
				failedLease, failures, cleanupErr = "", 0, nil
			} else if response.Run != nil {
				// Revalidate after the last failed RPC before fencing the physical owner:
				// finalization/capture may have reconciled this member in the meantime.
				if failures >= 3 {
					if _, held := checkout.runCleanupHeld(cleanupVersion, true); held {
						failedLease, failures, cleanupErr = "", 0, nil
						continue
					}
					return computerMountFailure{code: "computer_program_cleanup_failed", err: fmt.Errorf("reconcile Program processes: %w", cleanupErr)}
				}
				cleanupCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
				cleanupErr = m.cleanupComputerRun(cleanupCtx, machine, mount, *response.Run)
				cancel()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if _, held := checkout.runCleanupHeld(cleanupVersion, false); held {
					failedLease, failures, cleanupErr = "", 0, nil
				} else if cleanupErr != nil {
					failedLease = response.Run.RunLeaseID
					failures++
				} else {
					failedLease = ""
					failures = 0
					err = client.ReconcileComputerRun(ctx, workerapi.ComputerRunReconcileRequest{ComputerRunCleanupRequest: request, ComputerRunCleanup: *response.Run})
					if err == nil {
						continue
					}
				}
			}
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (m Server) cleanupComputerRun(ctx context.Context, machine vm.Machine, mount workerapi.ComputerInstanceAssignment, member workerapi.ComputerRunCleanup) error {
	if member.RunID == "" || member.RunLeaseID == "" || member.AttemptNumber == 0 {
		return errors.New("incomplete Program cleanup identity")
	}
	return guestControl{machine: machine}.cleanupRun(ctx, &computerv0.ComputerRunCleanupRequest{ComputerId: mount.ComputerID, ComputerInstanceId: mount.ComputerInstanceID, WriterGeneration: mount.WriterGeneration, ChannelCredential: m.channelCredential(mount), RunId: member.RunID, RunLeaseId: member.RunLeaseID, AttemptNumber: member.AttemptNumber})
}

// cleanupRun asks the guest to prove cleanup of one Program run. Cancellation
// closes the stream at any step and returns only after that close has finished.
func (g guestControl) cleanupRun(ctx context.Context, request *computerv0.ComputerRunCleanupRequest) error {
	var response computerv0.ComputerRunCleanupResponse
	if _, err := g.exchange(ctx, guestControlExchange{
		header:       wire.StreamHeader{Type: wire.StreamTypeComputerRunCleanup, RunID: request.GetRunId()},
		request:      request,
		response:     &response,
		cancellation: guestControlCancelAwaitStreamClose,
	}); err != nil {
		return err
	}
	if !response.Reconciled || response.Error != "" {
		return fmt.Errorf("program cleanup not proven: %s", response.Error)
	}
	return nil
}

// Capture holds the same physical owner across pause and abort. Its unreachable
// guest is not a failed cleanup proof. The final failure decision shares the
// capture ownership lock so capture cannot start between that decision and the
// Server's teardown. No lock is held across a guest RPC.
func (c *machineCheckout) runCleanupHeld(previousVersion int64, failed bool) (int64, bool) {
	if c == nil {
		return 0, false
	}
	p := c.machines
	p.mu.Lock()
	defer p.mu.Unlock()
	claim := p.claims[c.ref]
	if claim == nil || claim.gen != c.gen || claim.release != nil || claim.checkpointer != nil {
		return previousVersion, true
	}
	version := claim.entry.target.DesiredVersion
	// A whole abort may finish during the RPC; its advanced desired version
	// invalidates failures from the preceding capture even after the hold clears.
	if previousVersion != 0 && version != previousVersion {
		return version, true
	}
	if failed {
		claim.teardown = true
	}
	return version, false
}
