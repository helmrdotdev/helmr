package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// The hot-wait loop owns the only Program reader. Holding the renewal gate
// prevents a late guest renewal from overlapping the cgroup freeze. Control
// Plane renewals continue after the frozen transition until the physical owner
// finishes its operation.
func (task *guestRunLeaseTask) pauseComputerMember(ctx context.Context, wait WaitRequest, target workerapi.InstanceReconcileTarget, member workerapi.InstanceCaptureRun) error {
	task.renewalGate.Lock()
	defer task.renewalGate.Unlock()
	task.mu.Lock()
	lease := task.lease
	valid := !task.finished && !task.checkpointFrozen && lease.ExpiresAt.After(time.Now())
	task.mu.Unlock()
	if !valid || lease.ID != member.RunLeaseID || lease.RunID != member.RunID || lease.AttemptNumber != member.AttemptNumber || lease.ComputerInstanceID != target.ID || lease.WorkerEpoch != target.WorkerEpoch || lease.ComputerID != target.Source.ComputerID || lease.WriterGeneration != target.Source.WriterGeneration || wait.RunWaitID != member.RunWaitID || wait.CorrelationID == "" || wait.ResumeAttachID == "" || task.program.protocol == nil {
		return errors.New("computer member pause authority changed")
	}
	pauseCtx, cancel := context.WithDeadline(ctx, lease.ExpiresAt)
	defer cancel()
	protocol := task.program.protocol
	// Cancellation must unblock a stuck write or physical-frame read. Closing the
	// member stream does not claim that the shared VM has stopped.
	stop := context.AfterFunc(pauseCtx, func() { _ = protocol.Close() })
	defer stop()
	request := &programv0.CheckpointPauseRequest{RunId: lease.RunID, AttemptNumber: uint32(lease.AttemptNumber), RunLeaseId: lease.ID, RunWaitId: wait.RunWaitID, ResumeAttachId: wait.ResumeAttachID, CorrelationId: wait.CorrelationID, CheckpointId: target.Capture.CheckpointID, CheckpointRequestVersion: target.DesiredVersion, Execution: wait.Execution, TurnId: wait.TurnID}
	if err := wire.WriteCheckpointPauseRequest(protocol, request); err != nil {
		return err
	}
	if err := protocol.takePhysical(pauseCtx, task.processCheckpointRunEvent); err != nil {
		return err
	}
	header, size, err := wire.ReadStreamFrameHeader(protocol.reader)
	if err != nil {
		return err
	}
	if header.Type != wire.StreamTypeCheckpointPauseReady || size != 0 || header.RunWaitID != request.RunWaitId || header.CheckpointID != request.CheckpointId {
		return fmt.Errorf("computer member pause receipt differs from request")
	}
	if err := pauseCtx.Err(); err != nil {
		return err
	}
	task.markCheckpointFrozen()
	return nil
}
