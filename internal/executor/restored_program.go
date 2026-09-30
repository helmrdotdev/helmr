package executor

import (
	"context"
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

func (r ProgramRunner) startRestoredProgram(ctx context.Context, claim *workerapi.RunLeaseClaimResponse) (freshProgram, *programv0.ResumeAttach, error) {
	resume := claim.ProgramResume
	if resume == nil || resume.CheckpointID == "" || resume.RunWaitID == "" || (resume.EntrypointKind != "actor" && resume.EntrypointKind != "task") || len(claim.ProgramStart) != 0 || !claim.Lease.ExpiresAt.After(time.Now()) {
		return freshProgram{}, nil, errors.New("restored Program claim is incomplete or inconsistent")
	}
	attachCtx, cancel := context.WithDeadline(ctx, claim.Lease.ExpiresAt)
	defer cancel()
	var program freshProgram
	var attached *programv0.ResumeAttach
	err := retryRunLeaseRequest(attachCtx, func(attemptCtx context.Context) error {
		opened, err := r.Mounts.OpenChannel(attemptCtx, claim.Lease.ComputerInstanceID)
		if err != nil {
			return err
		}
		keep := false
		defer func() {
			if !keep {
				_ = opened.Channel.Close(context.Background())
			}
		}()
		if err := validateNewProgramMount(claim.Lease, opened.Mount); err != nil {
			return err
		}
		if opened.Mount.RestoreCheckpointID != resume.CheckpointID {
			return errors.New("program restore differs from local Computer")
		}
		authority := freshComputerAuthority(claim, opened.ChannelToken, opened.Mount)
		authority.Fence.BaseComputerDiskVersionId = claim.Lease.BaseComputerDiskVersionID
		attach, err := opened.GrantProgramResume(attemptCtx, &computerv0.GrantProgramResumeRequest{Authority: authority, RunWaitId: resume.RunWaitID, CheckpointId: resume.CheckpointID})
		if err != nil {
			return err
		}
		if (attach.Execution != nil) != (resume.EntrypointKind == "actor") {
			return errors.New("restored Program kind differs from Guest scope")
		}
		if attach.Execution != nil {
			if err := validateSessionExecution(attach.Execution, claim.Lease); err != nil {
				return err
			}
		}
		if attached != nil && !proto.Equal(attached, attach) {
			return errors.New("restored attachment changed during retry")
		}
		attached = attach
		stop := context.AfterFunc(attemptCtx, func() { _ = opened.Channel.Close(context.Background()) })
		defer stop()
		if err := frameio.WriteProtoFrame(opened.Channel.Stream(), attach); err != nil {
			return err
		}
		// Reconnect the retained wait without resolving its logical condition. The
		// durable acknowledgement below makes its current condition pollable again.
		decision := &programv0.ResumeDecision{Kind: "waiting", RequireConsumedAck: true, RunWaitId: attach.RunWaitId, CorrelationId: attach.CorrelationId, CheckpointId: attach.CheckpointId, ResumeAttachId: attach.ResumeAttachId, ResumeRequestVersion: attach.ResumeRequestVersion, RunLeaseId: attach.RunLeaseId}
		if err := frameio.WriteProtoFrame(opened.Channel.Stream(), decision); err != nil {
			return err
		}
		ack, err := readResumeAck(attemptCtx, opened.Channel)
		if err != nil {
			return err
		}
		expected := &programv0.ResumeAck{RunWaitId: attach.RunWaitId, CorrelationId: attach.CorrelationId, CheckpointId: attach.CheckpointId, ResumeAttachId: attach.ResumeAttachId, ResumeRequestVersion: attach.ResumeRequestVersion, RunLeaseId: attach.RunLeaseId}
		if !proto.Equal(ack, expected) {
			return errors.New("restored Program acknowledgement differs from attachment")
		}
		if err := (ControlPlaneRunWaits{Client: r.ControlPlane.Waits}).AcknowledgeRestore(attemptCtx, RestoreAcknowledgement{Lease: claim.Lease, RunWaitID: attach.RunWaitId, CheckpointID: attach.CheckpointId}); err != nil {
			return err
		}
		identity := &programv0.EntrypointIdentity{Kind: &programv0.EntrypointIdentity_Task{Task: &programv0.TaskEntrypoint{}}}
		if resume.EntrypointKind == "actor" {
			identity.Kind = &programv0.EntrypointIdentity_Actor{Actor: &programv0.ActorEntrypoint{}}
		}
		program = freshProgram{channel: opened.Channel, releaseSource: opened.ReleaseSource, mount: opened.Mount, lease: claim.Lease, authority: authority, execution: attach.Execution, entrypoint: identity}
		keep = true
		return nil
	})
	return program, attached, err
}

func (task *guestRunLeaseTask) continueRestoredWait(ctx context.Context, attach *programv0.ResumeAttach) error {
	request := WaitRequest{Execution: attach.Execution, TurnID: attach.TurnId, Leases: task, Computer: task.waitComputer, CorrelationID: attach.CorrelationId, RunWaitID: attach.RunWaitId, ResumeAttachID: attach.ResumeAttachId}
	request.Resume = func(ctx context.Context, decision WaitResumeDecision) error {
		if err := task.beforeWaitResume(ctx, decision); err != nil {
			return err
		}
		data := string(decision.Data)
		if data == "" {
			data = "null"
		}
		return wire.WriteResumeDecision(task.programStream(), &programv0.ResumeDecision{RunWaitId: attach.RunWaitId, CorrelationId: attach.CorrelationId, ResumeAttachId: attach.ResumeAttachId, Kind: decision.Kind, DataJson: data})
	}
	opened := workerapi.CreateRunWaitResponse{RunID: task.lease.RunID, RunWaitID: attach.RunWaitId, ResumeAttachID: attach.ResumeAttachId}
	return task.runHotWait(ctx, request, func(ctx context.Context, request WaitRequest) error {
		return task.runWaits().ContinueRunWait(ctx, request, opened)
	})
}
