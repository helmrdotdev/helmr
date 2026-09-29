package executor

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
func (m ComputerMaterializer) reconcileComputerRuns(ctx context.Context, session vm.Machine, mount workerapi.ComputerInstanceAssignment, client workerapi.ComputerMaterializerControlPlaneClient) error {
	request := workerapi.ComputerRunCleanupRequest{EnvironmentID: mount.EnvironmentID, ComputerInstanceID: mount.ComputerInstanceID, WriterGeneration: mount.WriterGeneration}
	var failedLease string
	var failures int
	var cleanupErr error
	for {
		response, err := client.GetComputerRunCleanup(ctx, request)
		if err == nil {
			if response.Run == nil || response.Run.RunLeaseID != failedLease {
				failedLease = ""
				failures = 0
				cleanupErr = nil
			}
			if response.Run != nil {
				// Revalidate after the last failed RPC before fencing the physical owner:
				// finalization/capture may have reconciled this member in the meantime.
				if failures >= 3 {
					return computerMountFailure{code: "computer_program_cleanup_failed", err: fmt.Errorf("reconcile Program processes: %w", cleanupErr)}
				}
				cleanupCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
				cleanupErr = m.cleanupComputerRun(cleanupCtx, session, mount, *response.Run)
				cancel()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if cleanupErr != nil {
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

func (m ComputerMaterializer) cleanupComputerRun(ctx context.Context, session vm.Machine, mount workerapi.ComputerInstanceAssignment, member workerapi.ComputerRunCleanup) error {
	if member.RunID == "" || member.RunLeaseID == "" || member.AttemptNumber == 0 {
		return errors.New("incomplete Program cleanup identity")
	}
	return guestControl{machine: session}.cleanupRun(ctx, &computerv0.ComputerRunCleanupRequest{ComputerId: mount.ComputerID, ComputerInstanceId: mount.ComputerInstanceID, WriterGeneration: mount.WriterGeneration, ChannelToken: m.channelToken(mount), RunId: member.RunID, RunLeaseId: member.RunLeaseID, AttemptNumber: member.AttemptNumber})
}

// cleanupRun asks the guest to prove cleanup of one Program run. Cancellation
// closes the stream at any step and returns only after that close has finished.
func (g guestControl) cleanupRun(ctx context.Context, request *computerv0.ComputerRunCleanupRequest) error {
	var response computerv0.ComputerRunCleanupResponse
	if err := g.exchange(ctx, guestControlExchange{
		header:        wire.StreamHeader{Type: wire.StreamTypeComputerRunCleanup, RunID: request.GetRunId()},
		request:       request,
		response:      &response,
		closeOnCancel: guestControlCloseOnCancelAwait,
	}); err != nil {
		return err
	}
	if !response.Reconciled || response.Error != "" {
		return fmt.Errorf("Program cleanup not proven: %s", response.Error)
	}
	return nil
}
