package computerhost

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/vm"
)

// liveCaptureMachine is an instance machine whose Computer can be cut live for a
// save. The allocation owner requires this capability for saving. A live cut leaves execution
// running during capture and upload. The concrete retained capture must support durable local
// source adoption after the CP commits its receipt.
type liveCaptureMachine interface {
	vm.Machine
	CaptureComputer(context.Context) (*vm.ComputerSnapshot, error)
}

func captureComputerSave(ctx context.Context, machine liveCaptureMachine, computerID string) (computerSaveCapture, error) {
	snapshot, err := machine.CaptureComputer(ctx)
	if err != nil {
		return nil, err
	}
	if snapshot == nil || snapshot.Capture == nil {
		return nil, &saveProtocolError{errors.New("live Computer capture is missing")}
	}
	capture := snapshot.Capture
	if snapshot.ComputerID != computerID {
		snapshot.Capture.Release()
		return nil, &saveProtocolError{errors.New("live Computer capture differs from save owner")}
	}
	return capture, nil
}
