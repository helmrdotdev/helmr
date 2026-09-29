package executor

import (
	"context"
	"errors"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
	"time"
)

type computerRestoreAcknowledger interface {
	AcknowledgeComputerRestore(context.Context, workerapi.ComputerRestoreAckRequest) (workerapi.ComputerRestoreAckResponse, error)
}

func activateRestoredComputerOnSession(ctx context.Context, session vm.Machine, control computerRestoreAcknowledger, request *computerv0.ComputerRestoreInstallation) error {
	if session == nil || control == nil || request == nil || request.Envelope == nil || request.CheckpointId == "" || request.DesiredVersion <= 0 {
		return errors.New("complete restore installation and control plane are required")
	}
	guest := guestControl{machine: session}
	if err := guest.sendRestoreInstallation(ctx, request, wire.StreamTypeComputerRestoreInstall); err != nil {
		return err
	}
	ack := workerapi.ComputerRestoreAckRequest{ComputerInstanceID: request.Envelope.ComputerInstanceId, CheckpointID: request.CheckpointId, DesiredVersion: request.DesiredVersion, WriterGeneration: int64(request.Envelope.WriterGeneration), Grants: make([]workerapi.ComputerRestoreGrant, 0, len(request.Grants))}
	for _, grant := range request.Grants {
		if grant == nil || grant.Fence == nil {
			return errors.New("restore grant is incomplete")
		}
		ack.Grants = append(ack.Grants, workerapi.ComputerRestoreGrant{RunID: grant.Fence.RunId, Lease: workerapi.RunLeaseFence{ID: grant.Fence.RunLeaseId, LeaseSequence: grant.Fence.LeaseSequence}})
	}
	receipt, err := control.AcknowledgeComputerRestore(ctx, ack)
	if err != nil {
		return err
	}
	if receipt.ComputerInstanceID != ack.ComputerInstanceID || receipt.CheckpointID != ack.CheckpointID || receipt.DesiredVersion != ack.DesiredVersion || receipt.WriterGeneration != ack.WriterGeneration {
		return errors.New("restore acknowledgement changed installation identity")
	}
	return guest.sendRestoreInstallation(ctx, request, wire.StreamTypeComputerRestoreActivate)
}

// sendRestoreInstallation delivers one installation phase. Cancellation closes
// the phase stream at any step of the exchange.
func (g guestControl) sendRestoreInstallation(ctx context.Context, request *computerv0.ComputerRestoreInstallation, kind wire.StreamType) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var response computerv0.ComputerRestoreInstallationResponse
	if _, err := g.exchange(ctx, guestControlExchange{
		header:       wire.StreamHeader{Type: kind, ComputerID: request.Envelope.ComputerId, ComputerInstanceID: request.Envelope.ComputerInstanceId, CheckpointID: request.CheckpointId},
		request:      request,
		response:     &response,
		cancellation: guestControlCancelCloseStreamAndRead,
	}); err != nil {
		return err
	}
	if !proto.Equal(response.Installation, request) {
		return errors.New("guest restore receipt changed installation")
	}
	return nil
}

type ComputerRestoreControl interface {
	computerRestoreAcknowledger
	GetComputerRestorePlan(context.Context, workerapi.ComputerRestorePlanRequest) (workerapi.ComputerRestorePlanResponse, error)
}

func (m ComputerMaterializer) activateRestore(ctx context.Context, session vm.Machine, mount workerapi.ComputerInstanceAssignment) error {
	var plan *workerapi.ComputerRestorePlan
	if err := retryControlRequest(ctx, func(ctx context.Context) error {
		response, err := m.RestoreControl.GetComputerRestorePlan(ctx, workerapi.ComputerRestorePlanRequest{EnvironmentID: mount.EnvironmentID, ComputerInstanceID: mount.ComputerInstanceID, WriterGeneration: mount.WriterGeneration})
		if err != nil {
			return err
		}
		if response.Plan == nil {
			return errors.New("Computer restore intent is not committed")
		}
		plan = response.Plan
		return nil
	}); err != nil {
		return err
	}
	if plan.ComputerInstanceID != mount.ComputerInstanceID || plan.ComputerID != mount.ComputerID || plan.CheckpointID != mount.RestoreCheckpointID || plan.WriterGeneration != mount.WriterGeneration || plan.WorkerEpoch != mount.RuntimeEpoch || plan.VMPlatformID != mount.VMPlatformID || plan.DesiredVersion != mount.DesiredVersion {
		return errors.New("restore plan differs from materialized Instance")
	}
	installation := &computerv0.ComputerRestoreInstallation{Envelope: &computerv0.ComputerOperationEnvelope{ComputerInstanceId: mount.ComputerInstanceID, ComputerId: mount.ComputerID, WriterGeneration: uint64(mount.WriterGeneration), ChannelToken: m.channelToken(mount)}, CheckpointId: plan.CheckpointID, DesiredVersion: plan.DesiredVersion}
	// The caller owns writer renewal and cancels ctx if that authority is lost.
	// Only member grants have immutable expiry during this installation.
	var deadline time.Time
	for _, member := range plan.Members {
		if member.AttemptNumber <= 0 || member.Lease.LeaseSequence <= 0 || member.ExpiresAt.IsZero() {
			return errors.New("restore plan has invalid member grant")
		}
		if deadline.IsZero() || member.ExpiresAt.Before(deadline) {
			deadline = member.ExpiresAt
		}
		installation.Grants = append(installation.Grants, &computerv0.ComputerRunAuthority{ChannelToken: m.channelToken(mount), WriteCapability: plan.WriteCapability, Fence: &computerv0.ComputerAuthorityFence{WorkerHostId: plan.WorkerHostID, WorkerEpoch: plan.WorkerEpoch, ComputerInstanceId: plan.ComputerInstanceID, ComputerId: plan.ComputerID, VmPlatformId: plan.VMPlatformID, WriterGeneration: plan.WriterGeneration, RunId: member.RunID, AttemptNumber: uint32(member.AttemptNumber), RunLeaseId: member.Lease.ID, LeaseSequence: member.Lease.LeaseSequence, BaseComputerDiskVersionId: member.BaseComputerDiskVersionID, ExpiresAtUnixNano: member.ExpiresAt.UnixNano()}})
	}
	activationCtx := ctx
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		activationCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	return retryControlRequest(activationCtx, func(ctx context.Context) error {
		return activateRestoredComputerOnSession(ctx, session, m.RestoreControl, installation)
	})
}
