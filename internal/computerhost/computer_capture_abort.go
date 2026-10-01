package computerhost

import (
	"context"
	"errors"
	"fmt"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// captureAbortProgress belongs to the active physical capture owner. A durable
// acknowledgment with no local activation proof cannot authorize a new source.
type captureAbortProgress struct {
	receipt        workerapi.CaptureAbortResponse
	guestCompleted bool
}

func validateCaptureAbortReceipt(target workerapi.InstanceReconcileTarget, response workerapi.CaptureAbortResponse) error {
	if response.WorkerHostID == "" || response.ComputerInstanceID != target.ID || response.WorkerEpoch != target.WorkerEpoch || response.DesiredVersion != target.DesiredVersion || response.CheckpointID != target.Capture.CheckpointID || response.ComputerID != target.Source.ComputerID || response.WriterGeneration != target.Source.WriterGeneration || response.MembershipRevision != target.Capture.MembershipRevision || response.VMPlatformID != target.Source.VMPlatformID {
		return fmt.Errorf("%w: capture abort receipt changed source", errForeignSourceReceipt)
	}
	switch response.Disposition {
	case workerapi.CaptureAdopted:
		if len(response.Members) != 0 {
			return errors.New("adopted capture returned member authority")
		}
	case workerapi.CaptureAborted, workerapi.CaptureAbortAcknowledged:
		if response.AbortDesiredVersion != target.DesiredVersion+1 {
			return fmt.Errorf("%w: capture abort receipt changed version", errForeignSourceReceipt)
		}
	default:
		return fmt.Errorf("%w: capture abort receipt has unknown disposition", errForeignSourceReceipt)
	}
	return nil
}

func (g guestControl) abortCapture(ctx context.Context, request *computerv0.ComputerCaptureAbortRequest) error {
	var response computerv0.ComputerCaptureAbortResponse
	_, err := g.exchange(ctx, guestControlExchange{header: wire.StreamHeader{Type: wire.StreamTypeComputerCaptureAbort, ComputerID: request.Capture.ComputerId, CheckpointID: request.Capture.CheckpointId}, request: request, response: &response, cancellation: guestControlCancelCloseStreamAndRead})
	if err != nil {
		return err
	}
	if response.CheckpointId != request.Capture.CheckpointId || response.AbortDesiredVersion != request.AbortDesiredVersion || response.Activated != request.Activate {
		return errors.New("guest capture abort acknowledgment changed identity")
	}
	return nil
}

// resumeCaptureStep is retried while the source remains held. Each retry reads
// current member grants and cancellation dispositions; no cached grant extends
// the Control Plane's authority. Installation precedes ordinary guest renewal,
// which precedes activation and the durable acknowledgment.
func (p *PreparedMachines) resumeCaptureStep(ctx context.Context, target workerapi.InstanceReconcileTarget, c *computerCheckpointer, state *captureAbortProgress) (adopted bool, err error) {
	response, err := p.Checkpoints.AbortCapture(ctx, workerapi.CaptureAbortRequest{ComputerInstanceID: target.ID, WorkerEpoch: target.WorkerEpoch, DesiredVersion: target.DesiredVersion, CheckpointID: target.Capture.CheckpointID})
	if err != nil {
		return false, err
	}
	if err = validateCaptureAbortReceipt(target, response); err != nil {
		return false, err
	}
	if response.Disposition == workerapi.CaptureAdopted {
		return true, nil
	}
	if response.Disposition == workerapi.CaptureAbortAcknowledged {
		if !state.guestCompleted {
			return false, errors.New("capture abort acknowledgment has no local source proof")
		}
		return false, nil
	}
	state.receipt = response
	if c.capture != nil && !state.guestCompleted {
		if err = c.capture.ResumeGuestControl(ctx); err != nil {
			return false, err
		}
	}
	capture, err := computerFreezeRequest(target)
	if err != nil {
		return false, err
	}
	members, err := p.ComputerCaptures.prepareAbortMembers(ctx, target, response, false)
	if err != nil {
		return false, err
	}
	request := &computerv0.ComputerCaptureAbortRequest{Capture: capture, AbortDesiredVersion: response.AbortDesiredVersion, Members: members}
	control := guestControl{machine: c.machine}
	if err = control.abortCapture(ctx, request); err != nil {
		return false, err
	}
	request.Members, err = p.ComputerCaptures.prepareAbortMembers(ctx, target, response, true)
	if err != nil {
		return false, err
	}
	request.Activate = true
	if err = control.abortCapture(ctx, request); err != nil {
		return false, err
	}
	if c.capture != nil && !state.guestCompleted {
		if err = c.capture.CompleteAbort(ctx); err != nil {
			return false, err
		}
	}
	state.guestCompleted = true
	if err = c.cleanupCheckpointStaging(); err != nil {
		return false, err
	}
	cancelled := []string{}
	for _, member := range response.Members {
		if member.Cancelled {
			cancelled = append(cancelled, member.Lease.ID)
		}
	}
	receipt, err := p.Checkpoints.CompleteCaptureAbort(ctx, workerapi.CaptureAbortCompleteRequest{CancelledRunLeaseIDs: cancelled, ComputerInstanceID: target.ID, WorkerEpoch: target.WorkerEpoch, DesiredVersion: target.DesiredVersion, CheckpointID: target.Capture.CheckpointID, AbortDesiredVersion: response.AbortDesiredVersion})
	if err != nil {
		return false, err
	}
	acknowledged := target
	acknowledged.DesiredVersion = response.AbortDesiredVersion
	if err = validateComputerCheckpointReceipt(acknowledged, receipt); err != nil {
		return false, err
	}
	return false, nil
}
