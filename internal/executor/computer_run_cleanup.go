package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// The Computer owner continues cleanup after a Run's own context/stream closes.
// A failed CP acknowledgement is retried with the same Guest receipt; inability
// to prove scoped termination instead requires the physical owner's failure path.
func (m ComputerMaterializer) reconcileComputerRuns(ctx context.Context, session vm.Session, mount workerapi.ComputerInstanceAssignment, client workerapi.ComputerMaterializerControlPlaneClient) error {
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

func (m ComputerMaterializer) cleanupComputerRun(ctx context.Context, session vm.Session, mount workerapi.ComputerInstanceAssignment, member workerapi.ComputerRunCleanup) error {
	if member.RunID == "" || member.RunLeaseID == "" || member.AttemptNumber == 0 {
		return errors.New("incomplete Program cleanup identity")
	}
	conn, err := session.OpenStream(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	closed := make(chan struct{})
	stopClose := context.AfterFunc(ctx, func() { defer close(closed); _ = conn.Close() })
	defer func() {
		if !stopClose() {
			<-closed
		}
	}()
	if err = wire.WriteStreamFrameHeader(conn, wire.StreamHeader{Type: wire.StreamTypeComputerRunCleanup, RunID: member.RunID}, 0); err != nil {
		return err
	}
	if err = frameio.WriteProtoFrame(conn, &computerv0.ComputerRunCleanupRequest{ComputerId: mount.ComputerID, ComputerInstanceId: mount.ComputerInstanceID, WriterGeneration: mount.WriterGeneration, ChannelToken: m.channelToken(mount), RunId: member.RunID, RunLeaseId: member.RunLeaseID, AttemptNumber: member.AttemptNumber}); err != nil {
		return err
	}
	var response computerv0.ComputerRunCleanupResponse
	if err = frameio.ReadProtoFrame(conn, &response); err != nil {
		return err
	}
	if !response.Reconciled || response.Error != "" {
		return fmt.Errorf("Program cleanup not proven: %s", response.Error)
	}
	return nil
}
