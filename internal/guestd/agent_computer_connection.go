package guestd

import (
	"context"
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
)

// Only the owned host transport can invoke Computer continuation control. No
// channel credential or fresh installation is forwarded to customer processes.
func handleAgentComputerConnection(ctx context.Context, connection programConnection, bodyLength uint64, computers *computerOperationRegistry) error {
	if bodyLength != 0 || computers == nil {
		return errors.New("computer continuation requires an empty body and registry")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := connection.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	var request agentv1.ComputerSessionControl
	if err := frameio.ReadProtoFrameBounded(connection, maxAgentTransportFrameBytes, &request); err != nil {
		return err
	}
	var receipt *agentv1.ComputerSessionReceipt
	var err error
	switch operation := request.GetOperation().(type) {
	case *agentv1.ComputerSessionControl_Capture:
		receipt, err = computers.captureAgentComputer(ctx, operation.Capture)
	case *agentv1.ComputerSessionControl_Install:
		var clock *computerAuthorityClock
		clock, err = observeComputerAuthority(connection)
		if err == nil {
			receipt, err = computers.installAgentComputer(ctx, operation.Install, false, clock)
		}
	case *agentv1.ComputerSessionControl_Activate:
		receipt, err = computers.installAgentComputer(ctx, operation.Activate, true, nil)
	case *agentv1.ComputerSessionControl_Controls:
		receipt, err = computers.applyAgentComputerControls(operation.Controls)
	case *agentv1.ComputerSessionControl_Inspect:
		receipt, err = computers.inspectAgentComputer(operation.Inspect)
	default:
		return errors.New("computer continuation operation is absent")
	}
	if receipt == nil {
		receipt = &agentv1.ComputerSessionReceipt{}
	}
	if err != nil {
		receipt.Error = err.Error()
		if errors.Is(err, errComputerCaptureAdmissionPending) {
			receipt.ErrorCode = agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_CAPTURE_ADMISSION_PENDING
		}
		if errors.Is(err, errComputerCaptureAbsentAfterExpiry) {
			receipt.ErrorCode = agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_CAPTURE_ABSENT_AFTER_EXPIRY
		}
		if request.GetInstall() != nil && !request.GetInstall().GetSourceAbort() && errors.Is(err, errComputerCaptureUnusable) {
			receipt.ErrorCode = agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_CAPTURE_UNUSABLE
		}
		if errors.Is(err, errComputerAuthorityExpired) || errors.Is(err, errSessionGrantExpired) {
			receipt.ErrorCode = agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_AUTHORITY_EXPIRED
		}
	}
	if err := connection.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return writeAgentTransportFrame(connection, receipt)
}
