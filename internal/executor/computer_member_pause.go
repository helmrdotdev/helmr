package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
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
	if err := ctx.Err(); err != nil {
		return err
	}
	// Once dispatched, join the pause receipt even if capture planning stops.
	// The renewal gate also keeps abort activation behind this I/O, so a late
	// pause cannot freeze a member after the source has resumed. Only expiry of
	// the confirmed grant can abandon the stream while the pause is in flight.
	pauseCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), lease.ExpiresAt)
	defer cancel()
	task.mu.Lock()
	task.capturePaused = true
	task.mu.Unlock()
	protocol := task.program.protocol
	// Grant expiry must unblock a stuck write or physical-frame read. Closing
	// the member stream does not claim that the shared VM has stopped.
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

func (task *guestRunLeaseTask) resumeCapturedMember(ctx context.Context, target workerapi.InstanceReconcileTarget, member workerapi.CaptureAbortMember, restoreRenewal bool) (*computerv0.ComputerRunAuthority, uint64, error) {
	task.renewalGate.Lock()
	defer task.renewalGate.Unlock()
	task.mu.Lock()
	defer task.mu.Unlock()
	lease := task.lease
	if task.finished || task.authority == nil || task.authority.Fence == nil || member.Cancelled || lease.ID != member.Lease.ID || lease.LeaseSequence != member.Lease.LeaseSequence || lease.RunID != member.RunID || lease.AttemptNumber != member.AttemptNumber || lease.BaseComputerDiskVersionID != member.BaseComputerDiskVersionID || !member.ExpiresAt.After(time.Now()) {
		return nil, 0, errors.New("capture abort member authority changed")
	}
	authority := proto.Clone(task.authority).(*computerv0.ComputerRunAuthority)
	authority.Fence.ExpiresAtUnixNano = member.ExpiresAt.UnixNano()
	if !restoreRenewal {
		return authority, 0, nil
	}
	if lease.ExpiresAt.After(member.ExpiresAt) {
		fence, err := task.mounts.RenewComputerAuthority(ctx, &computerv0.RenewComputerAuthorityRequest{Previous: authority, NewExpiresAtUnixNano: lease.ExpiresAt.UnixNano()})
		if err != nil {
			return nil, 0, err
		}
		authority.Fence = fence
	} else {
		task.lease.ExpiresAt = member.ExpiresAt
	}
	task.authority = authority
	if task.captureCheckpoint != target.Capture.CheckpointID {
		task.captureCheckpoint = target.Capture.CheckpointID
		task.captureAttachSequence, task.capturePreparedSequence = 0, 0
	}
	if task.checkpointFrozen && task.capturePreparedSequence == 0 {
		opened, err := task.mounts.OpenChannel(ctx, lease.ComputerInstanceID)
		if err != nil {
			return nil, 0, err
		}
		keep := false
		defer func() {
			if !keep {
				_ = opened.Channel.Close(context.Background())
			}
		}()
		if err := validateNewProgramMount(lease, opened.Mount); err != nil {
			return nil, 0, err
		}
		if opened.ChannelCredential != authority.ChannelCredential {
			return nil, 0, errors.New("capture abort channel credential changed")
		}
		task.captureAttachSequence++
		q := &computerv0.ComputerCaptureAbortAttachRequest{CheckpointId: target.Capture.CheckpointID, AbortDesiredVersion: target.DesiredVersion + 1, Member: &computerv0.ComputerCaptureRun{RunId: member.RunID, RunWaitId: member.RunWaitID, RunLeaseId: member.Lease.ID, AttemptNumber: uint32(member.AttemptNumber)}, Authority: authority, AttachSequence: task.captureAttachSequence}
		attachCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		stop := context.AfterFunc(attachCtx, func() { _ = opened.Channel.Close(context.Background()) })
		defer stop()
		stream := opened.Channel.Stream()
		if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{Type: wire.StreamTypeComputerCaptureAbortAttach}, 0); err != nil {
			return nil, 0, err
		}
		if err := frameio.WriteProtoFrame(stream, q); err != nil {
			return nil, 0, err
		}
		var ack computerv0.ComputerCaptureAbortAttachResponse
		if err := frameio.ReadProtoFrame(stream, &ack); err != nil {
			return nil, 0, err
		}
		if ack.CheckpointId != q.CheckpointId || ack.AbortDesiredVersion != q.AbortDesiredVersion || !proto.Equal(ack.Member, q.Member) || ack.AttachSequence != q.AttachSequence {
			return nil, 0, errors.New("capture abort attachment acknowledgment changed")
		}
		if !stop() {
			return nil, 0, attachCtx.Err()
		}
		if err := task.program.protocol.replacePausedStream(ctx, stream); err != nil {
			return nil, 0, err
		}
		// The stable protocol owns the replacement stream; the borrowed channel
		// has no VM lifetime ownership. Its old deferred Close remains harmless.
		keep = true
		task.capturePreparedSequence = q.AttachSequence
	}
	task.checkpointFrozen = false
	return proto.Clone(authority).(*computerv0.ComputerRunAuthority), task.capturePreparedSequence, nil
}
